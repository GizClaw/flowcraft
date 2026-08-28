package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils"
)

// Builder constructs a deployment's resources from a [Document] using
// an explicit [resource.Registry]. The registry is owned by the caller;
// Builder never touches global state.
type Builder struct {
	registry *resource.Registry
	loader   *resource.Loader
	resolver *resource.ReferenceResolver
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithLoader supplies the deployment-level loader used to materialize
// settings subtrees that are {"file":…} / {"embed":…} references, and
// passed to factories for their own source resolution.
func WithLoader(loader *resource.Loader) BuilderOption {
	return func(b *Builder) { b.loader = loader }
}

// WithResolver supplies the reference resolver used to expand inline
// ${scheme:ref} strings in every settings subtree (resources, agent
// engine, agent hooks) before a factory decodes them. When unset, the
// builder uses an all-open default resolver: env + home, plus base when
// the loader declares a base directory.
func WithResolver(resolver *resource.ReferenceResolver) BuilderOption {
	return func(b *Builder) { b.resolver = resolver }
}

// NewBuilder returns a Builder over registry. A nil registry yields an
// empty one.
func NewBuilder(registry *resource.Registry, opts ...BuilderOption) *Builder {
	if registry == nil {
		registry = resource.NewRegistry()
	}
	builder := &Builder{registry: registry}
	for _, opt := range opts {
		if opt != nil {
			opt(builder)
		}
	}
	return builder
}

// Build validates the document, resolves the resource DAG, and
// constructs every resource in dependency order. On failure, values
// already built are closed in reverse construction order (rollback).
func (b *Builder) Build(ctx context.Context, doc Document) (*Result, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	graph := resource.NewGraph()
	for name, res := range doc.Resources {
		if err := graph.Add(name, res); err != nil {
			return nil, err
		}
	}
	order, err := graph.TopoOrder()
	if err != nil {
		return nil, err
	}

	values := make(map[string]any, len(order))
	// Secret stores are built first: their settings never reference
	// secrets, and once assembled they feed the ${secret:...} scheme
	// every other resource's expansion uses.
	stores, defaultStore, err := b.buildSecretStores(ctx, doc, order, values, b.resolver)
	if err != nil {
		logCleanup(ctx, values, order)
		return nil, err
	}
	resolver, secrets, err := b.effectiveResolver(stores, defaultStore)
	if err != nil {
		logCleanup(ctx, values, order)
		return nil, err
	}
	for _, name := range order {
		if _, prebuilt := values[name]; prebuilt {
			continue
		}
		value, err := b.buildResource(ctx, name, doc.Resources[name], values, resolver, secrets)
		if err != nil {
			logCleanup(ctx, values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		values[name] = value
	}

	return &Result{
		values: values,
		order:  order,
		agents: make(map[string]*agent.Agent, len(doc.Agents)),
	}, nil
}

// buildResource constructs one resource value: loader materialization,
// inline reference expansion, dependency resolution, and factory
// invocation. Values already built (secret stores, earlier deps) are
// read from values.
func (b *Builder) buildResource(
	ctx context.Context,
	name string,
	res resource.Resource,
	values map[string]any,
	resolver *resource.ReferenceResolver,
	secrets *resource.SecretResolver,
) (any, error) {
	factory, ok := b.registry.Lookup(res.Kind, res.Impl)
	if !ok {
		return nil, errdefs.Validationf(
			"deploy: resource %q: no factory for %s/%s",
			name, res.Kind, res.Impl)
	}
	if err := validateDeps(factory, res.Deps); err != nil {
		return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
	}
	settings := res.Settings
	var err error
	if b.loader != nil {
		settings, err = resolveSettings(ctx, b.loader, settings)
		if err != nil {
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
	}
	settings, err = expandSettings(ctx, resolver, b.loader, settings)
	if err != nil {
		return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
	}
	deps, err := resolveDeps(values, res.Deps)
	if err != nil {
		return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
	}
	return factory.New(ctx, resource.Input{
		Settings: settings,
		Deps:     deps,
		Loader:   b.loader,
		Secrets:  secrets,
	})
}

// buildSecretStores constructs every secret.Store resource ahead of the
// main build pass. Their own settings expand with the base resolver
// (env/home/base only — stores cannot reference secrets), and the
// built values are recorded in values so other resources can declare
// them as deps.
func (b *Builder) buildSecretStores(
	ctx context.Context,
	doc Document,
	order []string,
	values map[string]any,
	resolver *resource.ReferenceResolver,
) (map[string]resource.SecretStore, string, error) {
	stores := make(map[string]resource.SecretStore)
	ids := make(map[string]string)
	defaultStore := ""
	for _, name := range order {
		res := doc.Resources[name]
		if res.Kind != secret.ResourceKind {
			continue
		}
		value, err := b.buildResource(ctx, name, res, values, resolver, nil)
		if err != nil {
			return nil, "", errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		store, ok := value.(resource.SecretStore)
		if !ok {
			return nil, "", errdefs.Validationf(
				"deploy: resource %q: %s/%s did not produce a secret store",
				name, res.Kind, res.Impl)
		}
		id := name
		if m, ok := value.(resource.SecretStoreID); ok && m.SecretStoreID() != "" {
			id = m.SecretStoreID()
		}
		if previous, exists := ids[id]; exists {
			return nil, "", errdefs.Validationf(
				"deploy: duplicate secret store id %q for resources %q and %q",
				id, previous, name)
		}
		if store.DefaultSecretStore() {
			if defaultStore != "" {
				return nil, "", errdefs.Validationf(
					"deploy: more than one default secret store: %q and %q",
					defaultStore, id)
			}
			defaultStore = id
		}
		values[name] = value
		ids[id] = name
		stores[id] = store
	}
	return stores, defaultStore, nil
}

// effectiveResolver returns the expansion resolver for the main build
// pass: the builder's injected resolver (or the all-open default), plus
// the ${secret:...} scheme assembled from the built stores (each wrapped
// in a TTL cache for per-request resolution). The second return is the
// resolver handed to factories for lazy Secret.Resolve calls.
func (b *Builder) effectiveResolver(
	stores map[string]resource.SecretStore,
	defaultStore string,
) (*resource.ReferenceResolver, *resource.SecretResolver, error) {
	base := b.resolver
	if base == nil {
		schemes := []resource.Scheme{
			resource.EnvScheme(os.LookupEnv),
			resource.HomeScheme(),
		}
		if b.loader != nil && b.loader.BaseDir() != "" {
			schemes = append(schemes, resource.BaseScheme(b.loader.BaseDir()))
		}
		base = resource.NewResolver(schemes...)
	}
	if len(stores) == 0 {
		return base, nil, nil
	}
	cached := make(map[string]resource.SecretStore, len(stores))
	for id, store := range stores {
		cached[id] = resource.NewCachingSecretStore(store, secretCacheTTL)
	}
	secrets := resource.NewSecretResolver(cached, defaultStore)
	return base.WithScheme(secrets.Scheme()), secrets, nil
}

// secretCacheTTL bounds how long lazy secret lookups reuse a cached
// value before hitting the backend again.
const secretCacheTTL = time.Minute

// resolveSettings materializes a settings subtree when the whole
// subtree is a file/embed reference; inline settings pass through.
func resolveSettings(
	ctx context.Context,
	loader *resource.Loader,
	raw []byte,
) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	src, err := resource.ParseSource(raw)
	if err != nil {
		return nil, err
	}
	if !src.IsRef() {
		return raw, nil
	}
	data, err := loader.Load(ctx, src)
	if err != nil {
		return nil, err
	}
	// Settings sub-documents may be YAML; convert to JSON so factory
	// DecodeSettings (strict JSON) accepts them.
	return utils.ToJSON(data)
}

// expandSettings expands inline ${scheme:ref} strings in one settings
// subtree with the given resolver. A nil resolver falls back to the
// all-open default: env + home, plus base when the loader declares a
// base directory. Expansion is strict — an unknown scheme, a missing
// env variable, or an unterminated reference fails the build.
func expandSettings(
	ctx context.Context,
	resolver *resource.ReferenceResolver,
	loader *resource.Loader,
	raw []byte,
) ([]byte, error) {
	if resolver == nil {
		schemes := []resource.Scheme{
			resource.EnvScheme(os.LookupEnv),
			resource.HomeScheme(),
		}
		if loader != nil && loader.BaseDir() != "" {
			schemes = append(schemes, resource.BaseScheme(loader.BaseDir()))
		}
		resolver = resource.NewResolver(schemes...)
	}
	expanded, err := resource.Expand(ctx, raw, resource.WithResolver(resolver))
	if err != nil {
		return nil, err
	}
	return expanded, nil
}

// Wire runs the post-build wiring phase: resource values implementing
// [resource.Wireable] attach themselves (observers to buses, hooks to
// streams), then every agent is bound from its engine and hooks, and
// finally values implementing [resource.DeploymentBinder] receive the
// assembled deployment (agents included). Wire never participates in
// the construction DAG, so observed values never depend on their
// observers.
func (b *Builder) Wire(ctx context.Context, result *Result, doc Document) error {
	if result == nil {
		return errdefs.Validationf("deploy: wire: nil result")
	}
	for _, name := range result.order {
		value, ok := result.values[name]
		if !ok {
			continue
		}
		if w, ok := value.(resource.Wireable); ok {
			if err := w.Wire(ctx); err != nil {
				return errdefs.Validationf(
					"deploy: wire resource %q: %v", name, err)
			}
		}
	}
	if err := b.bindAgents(ctx, result, doc); err != nil {
		return err
	}
	return b.bindDeployment(result)
}

// bindDeployment hands the fully assembled result to every resource
// value that needs it after agents are bound.
func (b *Builder) bindDeployment(result *Result) error {
	for _, name := range result.order {
		value, ok := result.values[name]
		if !ok {
			continue
		}
		if binder, ok := value.(resource.DeploymentBinder); ok {
			if err := binder.BindDeployment(result); err != nil {
				return errdefs.Validationf(
					"deploy: bind deployment resource %q: %v", name, err)
			}
		}
	}
	return nil
}

// Deploy is the convenience entry point: Build, then Wire. On wire
// failure every built value is closed before returning.
func (b *Builder) Deploy(ctx context.Context, doc Document) (*Result, error) {
	result, err := b.Build(ctx, doc)
	if err != nil {
		return nil, err
	}
	if err := b.Wire(ctx, result, doc); err != nil {
		if cerr := result.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "deploy: close built deployment after wire failure", cerr)
		}
		return nil, err
	}
	return result, nil
}

// logCleanup rolls back partially built resources on a Build failure
// and leaves any close error visible to telemetry.
func logCleanup(ctx context.Context, values map[string]any, order []string) {
	if err := closeAll(values, order, nil); err != nil {
		telemetry.WarnErr(ctx, "deploy: close partially built resources after failure", err)
	}
}

// bindAgents constructs every [agent.Agent] from its Definition and
// records it on the result.
func (b *Builder) bindAgents(ctx context.Context, result *Result, doc Document) error {
	for name, def := range doc.Agents {
		instance, err := BindAgent(ctx, b.registry, result, b.loader, b.resolver, name, def)
		if err != nil {
			return err
		}
		result.agents[name] = instance
	}
	return nil
}

// BindAgent assembles one [agent.Agent] from its Definition: the engine
// from the registry (kind = EngineRef.Kind / Impl), then each hook under
// "hook.<slot>", wiring [resource.Wireable] hooks before recording them
// on the agent. It never mutates result (result.agents stays untouched),
// so callers decide where the assembled agent is recorded — deployment
// build records it on the Result, while core/runtime registers it in the
// live agent registry.
//
// On failure, every component already constructed (engine and any wired
// hooks) is closed in reverse order so a partial assembly never leaks
// attached subscriptions or other resources.
func BindAgent(
	ctx context.Context,
	reg *resource.Registry,
	result *Result,
	loader *resource.Loader,
	resolver *resource.ReferenceResolver,
	name string,
	def agent.Definition,
) (*agent.Agent, error) {
	if reg == nil {
		return nil, errdefs.Validationf(
			"deploy: agent %q: resource registry is required", name)
	}
	engineFactory, ok := reg.Lookup(def.Engine.Kind, def.Engine.Impl)
	if !ok {
		return nil, errdefs.Validationf(
			"deploy: agent %q: no factory for engine %s/%s",
			name, def.Engine.Kind, def.Engine.Impl)
	}
	if err := validateDeps(engineFactory, def.Engine.Deps); err != nil {
		return nil, errdefs.Validationf("deploy: agent %q engine: %v", name, err)
	}
	engineDeps, err := resolveDeps(result.values, def.Engine.Deps)
	if err != nil {
		return nil, errdefs.Validationf("deploy: agent %q: %v", name, err)
	}

	var constructed []any
	fail := func(err error) (*agent.Agent, error) {
		closeAgentParts(constructed)
		return nil, err
	}

	engineSettings, err := expandSettings(ctx, resolver, loader, def.Engine.Settings)
	if err != nil {
		return fail(errdefs.Validationf(
			"deploy: agent %q engine: %v", name, err))
	}
	engine, err := engineFactory.New(ctx, resource.Input{
		Settings: engineSettings,
		Deps:     engineDeps,
		Loader:   loader,
	})
	if err != nil {
		return fail(errdefs.Validationf("deploy: agent %q engine: %v", name, err))
	}
	engineContract, ok := engine.(agent.Engine)
	if !ok {
		return fail(errdefs.Validationf(
			"deploy: agent %q: engine factory returned %T, want agent.Engine",
			name, engine))
	}
	constructed = append(constructed, engineContract)

	prepare, err := buildHookList[agent.Preparer](
		reg, loader, resolver, result, ctx, name, agent.HookSlotPreparer, def.Prepare)
	if err != nil {
		return fail(err)
	}
	constructed = appendAny(constructed, prepare)

	observe, err := buildHookList[agent.Observer](
		reg, loader, resolver, result, ctx, name, agent.HookSlotObserver, def.Observe)
	if err != nil {
		return fail(err)
	}
	constructed = appendAny(constructed, observe)

	referees, err := buildHookList[agent.Referee](
		reg, loader, resolver, result, ctx, name, agent.HookSlotReferee, def.Referees)
	if err != nil {
		return fail(err)
	}
	constructed = appendAny(constructed, referees)

	commit, err := buildHookList[agent.Committer](
		reg, loader, resolver, result, ctx, name, agent.HookSlotCommitter, def.Commit)
	if err != nil {
		return fail(err)
	}

	policy := agent.Policy{}
	if def.Policy != nil {
		policy = *def.Policy
	}
	return &agent.Agent{
		ID:       name,
		Card:     def.Card,
		Tools:    def.Tools,
		Policy:   policy,
		Engine:   engineContract,
		Prepare:  prepare,
		Observe:  observe,
		Referees: referees,
		Commit:   commit,
	}, nil
}

// buildHookList constructs every hook in one slot, type-asserting the
// factory values to T (agent.Preparer / Observer / Referee /
// Committer) and wiring [resource.Wireable] values before recording them
// on the agent. On failure every constructed hook is closed in reverse
// order.
func buildHookList[T any](
	reg *resource.Registry,
	loader *resource.Loader,
	resolver *resource.ReferenceResolver,
	result *Result,
	ctx context.Context,
	name, slot string,
	entries []agent.Hook,
) ([]T, error) {
	var values []T
	var constructed []any
	fail := func(err error) ([]T, error) {
		closeAgentParts(constructed)
		return nil, err
	}
	for i, entry := range entries {
		kind := resource.Kind("hook." + slot)
		factory, ok := reg.Lookup(kind, entry.Type)
		if !ok {
			return fail(errdefs.Validationf(
				"deploy: agent %q: no factory for hook %s/%s",
				name, kind, entry.Type))
		}
		if err := validateDeps(factory, entry.Deps); err != nil {
			return fail(errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err))
		}
		deps, err := resolveDeps(result.values, entry.Deps)
		if err != nil {
			return fail(errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err))
		}
		hookSettings, err := expandSettings(ctx, resolver, loader, entry.Settings)
		if err != nil {
			return fail(errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v",
				name, slot, i, err))
		}
		value, err := factory.New(ctx, resource.Input{
			Settings: hookSettings,
			Deps:     deps,
			Loader:   loader,
		})
		if err != nil {
			return fail(errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err))
		}
		constructed = append(constructed, value)
		typed, ok := value.(T)
		if !ok {
			return fail(errdefs.Validationf(
				"deploy: agent %q: hook %s[%d] factory returned %T, want %T",
				name, slot, i, value, *new(T)))
		}
		if w, ok := value.(resource.Wireable); ok {
			if err := w.Wire(ctx); err != nil {
				return fail(errdefs.Validationf(
					"deploy: agent %q: wire hook %s[%d]: %v", name, slot, i, err))
			}
		}
		values = append(values, typed)
	}
	return values, nil
}

func appendAny[T any](dst []any, values []T) []any {
	for _, value := range values {
		dst = append(dst, value)
	}
	return dst
}

// closeAgentParts closes every io.Closer among values in reverse order,
// best-effort, skipping nil and typed-nil entries.
func closeAgentParts(values []any) {
	for i := len(values) - 1; i >= 0; i-- {
		value := values[i]
		if value == nil || isNilValue(value) {
			continue
		}
		if closer, ok := value.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				telemetry.WarnErr(context.Background(),
					"deploy: close agent part failed", err)
			}
		}
	}
}

// validateDeps checks document deps against the factory's declared
// DepSpecs: every document key must match a fixed dep name or a Many
// dep prefix, and every Required dep must be supplied.
func validateDeps(factory resource.Factory, deps resource.Deps) error {
	spec := factory.Spec()
	declared := make(map[string]resource.DepSpec, len(spec.Deps))
	for _, dep := range spec.Deps {
		declared[dep.Name] = dep
	}
	for key := range deps {
		if _, ok := declared[key]; ok {
			continue
		}
		matched := false
		for _, dep := range spec.Deps {
			if dep.Many && strings.HasPrefix(key, dep.Name+".") {
				matched = true
				break
			}
		}
		if !matched {
			return errdefs.Validationf(
				"undeclared dep %q for %s/%s", key, spec.Kind, spec.Impl)
		}
	}
	for _, dep := range spec.Deps {
		if !dep.Required {
			continue
		}
		if dep.Many {
			found := false
			for key := range deps {
				if key == dep.Name || strings.HasPrefix(key, dep.Name+".") {
					found = true
					break
				}
			}
			if !found {
				return errdefs.Validationf(
					"required many dep %q missing for %s/%s",
					dep.Name, spec.Kind, spec.Impl)
			}
		} else if _, ok := deps[dep.Name]; !ok {
			return errdefs.Validationf(
				"required dep %q missing for %s/%s",
				dep.Name, spec.Kind, spec.Impl)
		}
	}
	return nil
}

// resolveDeps maps each declared dep to the built value, resolving
// "resource/item" refs through the container's [resource.ItemResolver].
func resolveDeps(values map[string]any, deps resource.Deps) (map[string]any, error) {
	resolved := make(map[string]any, len(deps))
	for name, ref := range deps {
		value, ok := values[ref.ResourceName()]
		if !ok {
			return nil, errdefs.Validationf(
				"dep %q references unbuilt resource %q", name, ref.ResourceName())
		}
		if item, hasItem := ref.ItemName(); hasItem {
			resolver, ok := value.(resource.ItemResolver)
			if !ok {
				return nil, errdefs.Validationf(
					"dep %q: resource %q does not expose items", name, ref.ResourceName())
			}
			itemValue, ok := resolver.ResolveItem(item)
			if !ok {
				return nil, errdefs.Validationf(
					"dep %q: resource %q has no item %q", name, ref.ResourceName(), item)
			}
			value = itemValue
		}
		resolved[name] = value
	}
	return resolved, nil
}

// Result owns the built resource values in construction order.
// The caller closes it (or the runtime layer closes it) when done.
type Result struct {
	values   map[string]any
	order    []string
	agents   map[string]*agent.Agent
	detached map[string]struct{}
}

// Detach marks resource names as caller-owned: Result.Close will not
// close them, and the caller takes over their lifecycle. It is used by
// the runtime to reject a reload whose event bus factory returned the
// current generation's bus without letting the aborted result close a
// bus another generation still uses.
func (r *Result) Detach(names ...string) {
	if r == nil || len(names) == 0 {
		return
	}
	if r.detached == nil {
		r.detached = make(map[string]struct{}, len(names))
	}
	for _, name := range names {
		r.detached[name] = struct{}{}
	}
}

func (r *Result) isDetached(name string) bool {
	if r == nil || r.detached == nil {
		return false
	}
	_, ok := r.detached[name]
	return ok
}

// Value returns the built resource registered under name.
func (r *Result) Value(name string) (any, bool) {
	v, ok := r.values[name]
	return v, ok
}

// Names returns the sorted built resource names.
func (r *Result) Names() []string {
	names := make([]string, 0, len(r.values))
	for name := range r.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Agent returns the assembled agent registered under name.
func (r *Result) Agent(name string) (*agent.Agent, bool) {
	a, ok := r.agents[name]
	return a, ok
}

// AgentNames returns the sorted bound agent names.
func (r *Result) AgentNames() []string {
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Close closes every io.Closer resource value in reverse construction
// order, then closes every bound agent (engine and lifecycle hooks).
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if err := closeAll(r.values, r.order, r); err != nil {
		errs = append(errs, err)
	}
	for _, name := range r.AgentNames() {
		instance := r.agents[name]
		if instance == nil {
			continue
		}
		if err := instance.Close(); err != nil {
			errs = append(errs, fmt.Errorf("deploy: close agent %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func closeAll(values map[string]any, order []string, result *Result) error {
	var errs []error
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		if result != nil && result.isDetached(name) {
			continue
		}
		value, ok := values[name]
		if !ok {
			continue
		}
		closer, ok := value.(io.Closer)
		if !ok {
			continue
		}
		if isNilValue(closer) {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
