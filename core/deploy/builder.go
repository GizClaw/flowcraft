package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Builder constructs a deployment's resources from a [Document] using
// an explicit [resource.Registry]. The registry is owned by the caller;
// Builder never touches global state.
type Builder struct {
	registry            *resource.Registry
	loader              *resource.Loader
	resolver            *resource.ReferenceResolver
	secretCacheTTL      time.Duration
	secretCacheCapacity int
	externalResources   []ExternalResource
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithLoader supplies the deployment-level loader used to materialize
// settings subtrees that are {"file":…} / {"embed":…} references, and
// passed to factories for their own source resolution.
func WithLoader(loader *resource.Loader) BuilderOption {
	return func(b *Builder) { b.loader = loader }
}

// WithResolver adds custom schemes to the expansion used for inline
// ${scheme:ref} strings in every settings subtree (resources, agent
// engine, agent hooks) before a factory decodes them. The custom
// schemes are merged on top of the all-open default resolver (env +
// home, plus base when the loader declares a base directory); a custom
// scheme with the same name overrides the built-in one.
func WithResolver(resolver *resource.ReferenceResolver) BuilderOption {
	return func(b *Builder) { b.resolver = resolver }
}

// WithSecretCacheTTL sets how long lazy secret lookups reuse a cached
// value before hitting the backend again. Zero keeps the default (one
// minute); a negative value disables caching.
func WithSecretCacheTTL(ttl time.Duration) BuilderOption {
	return func(b *Builder) { b.secretCacheTTL = ttl }
}

// WithSecretCacheCapacity bounds how many secret names the lazy-lookup
// cache holds at once. Zero keeps the default (see
// [resource.WithCacheCapacity]); raise it for a deployment that
// resolves more distinct names than that within one TTL window, so
// they are not re-fetched from the backend.
func WithSecretCacheCapacity(n int) BuilderOption {
	return func(b *Builder) { b.secretCacheCapacity = n }
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
//
// Build is [Builder.Plan] plus [Plan.Build]: a caller that wants to report
// configuration errors before constructing anything (a preflight, a CLI
// --check, a config validator) calls Plan directly.
func (b *Builder) Build(ctx context.Context, doc Document) (*Result, error) {
	plan, err := b.Plan(doc)
	if err != nil {
		return nil, err
	}
	return plan.Build(ctx)
}

// buildResource constructs one resource value: loader materialization,
// inline reference expansion, dependency resolution, and factory
// invocation. Values already built (secret stores, earlier deps) are
// read from values.
//
// Dependency declarations and their types were checked by [Builder.Plan];
// this method only assembles the values it reads.
func (p *Plan) buildResource(
	ctx context.Context,
	name string,
	values map[string]any,
	resolver *resource.ReferenceResolver,
	secrets *resource.SecretResolver,
) (any, error) {
	res := p.doc.Resources[name]
	entry, ok := p.entries[name]
	if !ok {
		return nil, errdefs.Validationf("no factory for %s/%s", res.Kind, res.Impl)
	}
	settings := res.Settings
	var err error
	if p.builder.loader != nil {
		settings, err = resolveSettings(ctx, p.builder.loader, settings)
		if err != nil {
			return nil, err
		}
	}
	settings, err = expandSettings(ctx, resolver, p.builder.loader, settings)
	if err != nil {
		return nil, err
	}
	deps, err := resolveDeps(values, p.externals, res.Deps)
	if err != nil {
		return nil, err
	}
	return entry.Build(ctx,
		resource.NewInput(settings, deps, p.builder.loader, secrets))
}

// buildSecretStores constructs every secret.Store resource ahead of the
// main build pass. Their own settings expand with the base resolver
// (env/home/base only — stores cannot reference secrets), and the
// built values are recorded in values so other resources can declare
// them as deps.
func (p *Plan) buildSecretStores(
	ctx context.Context,
	order []string,
	values map[string]any,
) (map[string]resource.SecretStore, string, error) {
	resolver := p.builder.resolver
	stores := make(map[string]resource.SecretStore)
	ids := make(map[string]string)
	defaultStore := ""
	for _, name := range order {
		res := p.doc.Resources[name]
		if res.Kind != secret.ResourceKind {
			continue
		}
		value, err := p.buildResource(ctx, name, values, resolver, nil)
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
// pass: the all-open default resolver merged with the builder's
// injected schemes, plus the ${secret:...} scheme assembled from the
// built stores (each wrapped in a TTL cache for per-request
// resolution). The second return is the resolver handed to factories
// for lazy Secret.Resolve calls.
func (b *Builder) effectiveResolver(
	stores map[string]resource.SecretStore,
	defaultStore string,
) (*resource.ReferenceResolver, *resource.SecretResolver, error) {
	base := defaultExpansionResolver(b.loader)
	if b.resolver != nil {
		base = base.Merge(b.resolver)
	}
	if len(stores) == 0 {
		return base, nil, nil
	}
	cached := make(map[string]resource.SecretStore, len(stores))
	ttl := b.secretCacheTTL
	if ttl == 0 {
		ttl = defaultSecretCacheTTL
	}
	for id, store := range stores {
		cached[id] = resource.NewCachingSecretStore(store, ttl,
			resource.WithCacheCapacity(b.secretCacheCapacity))
	}
	secrets := resource.NewSecretResolver(cached, defaultStore)
	return base.WithScheme(secrets.Scheme()), secrets, nil
}

// defaultSecretCacheTTL bounds how long lazy secret lookups reuse a
// cached value before hitting the backend again.
const defaultSecretCacheTTL = time.Minute

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
		resolver = defaultExpansionResolver(loader)
	}
	expanded, err := resource.Expand(ctx, raw, resource.WithResolver(resolver))
	if err != nil {
		return nil, err
	}
	return expanded, nil
}

// defaultExpansionResolver is the all-open expansion used when no
// resolver is injected: env + home, plus base when the loader declares
// a base directory. The agent board namespace is deferred — ${board:*}
// references resolve against agent.Board at execution time (graph node
// configs, script bridge), so deploy-time expansion passes them through
// verbatim (including the \${board:*} escaped form, whose backslash the
// agent layer consumes for its own literal escaping).
func defaultExpansionResolver(loader *resource.Loader) *resource.ReferenceResolver {
	schemes := []resource.Scheme{
		resource.EnvScheme(os.LookupEnv),
		resource.HomeScheme(),
	}
	if loader != nil && loader.BaseDir() != "" {
		schemes = append(schemes, resource.BaseScheme(loader.BaseDir()))
	}
	return resource.NewResolver(append(schemes,
		resource.PassthroughScheme{Prefix: agent.BoardRefPrefix},
	)...)
}

// Wire runs the post-build wiring phase: resource values implementing
// [resource.Wireable] attach themselves (observers to buses, hooks to
// streams), then every agent is bound from its engine and hooks, and
// finally values implementing [Binder] receive the assembled deployment
// (agents included). Wire never participates in the construction DAG, so
// observed values never depend on their observers.
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
		if binder, ok := value.(Binder); ok {
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
		instance, err := BindAgent(
			ctx, b.registry, result, b.loader, result.resolver, result.secrets, name, def)
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
	secrets *resource.SecretResolver,
	name string,
	def agent.Definition,
) (*agent.Agent, error) {
	if reg == nil {
		return nil, errdefs.Validationf(
			"deploy: agent %q: resource registry is required", name)
	}
	engineEntry, ok := reg.Lookup(def.Engine.Kind, def.Engine.Impl)
	if !ok {
		return nil, errdefs.Validationf(
			"deploy: agent %q: no factory for engine %s/%s",
			name, def.Engine.Kind, def.Engine.Impl)
	}
	// The plan checked these edges for a deployment build; BindAgent runs the
	// same checks again for an agent assembled after the build (core/runtime
	// dynamic registration), where no plan is in scope.
	producer := liveProducer(result)
	if err := checkProduces(engineEntry, reflect.TypeFor[agent.Engine](),
		fmt.Sprintf("deploy: agent %q engine", name)); err != nil {
		return nil, err
	}
	if err := checkDeps(engineEntry.Spec(), def.Engine.Deps, producer); err != nil {
		return nil, errdefs.Validationf("deploy: agent %q engine: %v", name, err)
	}
	engineDeps, err := resolveDeps(result.values, result.externals, def.Engine.Deps)
	if err != nil {
		return nil, errdefs.Validationf("deploy: agent %q: %v", name, err)
	}

	var constructed []io.Closer
	fail := func(err error) (*agent.Agent, error) {
		closeAgentParts(constructed)
		return nil, err
	}

	engineSettings, err := expandSettings(ctx, resolver, loader, def.Engine.Settings)
	if err != nil {
		return fail(errdefs.Validationf(
			"deploy: agent %q engine: %v", name, err))
	}
	engineValue, err := engineEntry.Build(ctx,
		resource.NewInput(engineSettings, engineDeps, loader, secrets))
	if err != nil {
		return fail(errdefs.Validationf("deploy: agent %q engine: %v", name, err))
	}
	engine, ok := engineValue.(agent.Engine)
	if !ok {
		return fail(errdefs.Validationf(
			"deploy: agent %q: engine factory returned %T, want agent.Engine",
			name, engineValue))
	}
	constructed = appendCloser(constructed, engine)

	prepare, err := buildHookList[agent.Preparer](
		ctx, reg, result, loader, resolver, secrets, name, agent.HookSlotPreparer, def.Prepare)
	if err != nil {
		return fail(err)
	}
	constructed = appendClosers(constructed, prepare)

	observe, err := buildHookList[agent.Observer](
		ctx, reg, result, loader, resolver, secrets, name, agent.HookSlotObserver, def.Observe)
	if err != nil {
		return fail(err)
	}
	constructed = appendClosers(constructed, observe)

	referees, err := buildHookList[agent.Referee](
		ctx, reg, result, loader, resolver, secrets, name, agent.HookSlotReferee, def.Referees)
	if err != nil {
		return fail(err)
	}
	constructed = appendClosers(constructed, referees)

	commit, err := buildHookList[agent.Committer](
		ctx, reg, result, loader, resolver, secrets, name, agent.HookSlotCommitter, def.Commit)
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
		Engine:   engine,
		Prepare:  prepare,
		Observe:  observe,
		Referees: referees,
		Commit:   commit,
	}, nil
}

// buildHookList constructs every hook in one slot, checking that the factory
// produces T (agent.Preparer / Observer / Referee / Committer) and that its
// dependencies are declared and type-correct, then wiring
// [resource.Wireable] values before recording them on the agent. On failure
// every constructed hook is closed in reverse order.
func buildHookList[T any](
	ctx context.Context,
	reg *resource.Registry,
	result *Result,
	loader *resource.Loader,
	resolver *resource.ReferenceResolver,
	secrets *resource.SecretResolver,
	name, slot string,
	entries []agent.Hook,
) ([]T, error) {
	var values []T
	var constructed []io.Closer
	fail := func(err error) ([]T, error) {
		closeAgentParts(constructed)
		return nil, err
	}
	want := reflect.TypeFor[T]()
	producer := liveProducer(result)
	for i, entry := range entries {
		where := fmt.Sprintf("deploy: agent %q: hook %s[%d]", name, slot, i)
		kind := resource.Kind("hook." + slot)
		hookEntry, ok := reg.Lookup(kind, entry.Type)
		if !ok {
			return fail(errdefs.Validationf(
				"%s: no factory for %s/%s", where, kind, entry.Type))
		}
		if err := checkProduces(hookEntry, want, where); err != nil {
			return fail(err)
		}
		if err := checkDeps(hookEntry.Spec(), entry.Deps, producer); err != nil {
			return fail(errdefs.Validationf("%s: %v", where, err))
		}
		deps, err := resolveDeps(result.values, result.externals, entry.Deps)
		if err != nil {
			return fail(errdefs.Validationf("%s: %v", where, err))
		}
		hookSettings, err := expandSettings(ctx, resolver, loader, entry.Settings)
		if err != nil {
			return fail(errdefs.Validationf("%s: %v", where, err))
		}
		value, err := hookEntry.Build(ctx,
			resource.NewInput(hookSettings, deps, loader, secrets))
		if err != nil {
			return fail(errdefs.Validationf("%s: %v", where, err))
		}
		constructed = appendCloser(constructed, value)
		typed, ok := value.(T)
		if !ok {
			return fail(errdefs.Validationf(
				"%s: factory returned %T, want %s", where, value, want))
		}
		if w, ok := value.(resource.Wireable); ok {
			if err := w.Wire(ctx); err != nil {
				return fail(errdefs.Validationf("%s: wire: %v", where, err))
			}
		}
		values = append(values, typed)
	}
	return values, nil
}

// appendCloser records one constructed part for rollback. A part that is not
// an io.Closer needs no teardown, so the rollback list stays a closure list
// instead of a list of any.
func appendCloser(dst []io.Closer, value any) []io.Closer {
	if value == nil || ptr.IsNil(value) {
		return dst
	}
	if closer, ok := value.(io.Closer); ok {
		return append(dst, closer)
	}
	return dst
}

func appendClosers[T any](dst []io.Closer, values []T) []io.Closer {
	for _, value := range values {
		dst = appendCloser(dst, value)
	}
	return dst
}

// closeAgentParts closes every constructed part in reverse order,
// best-effort.
func closeAgentParts(closers []io.Closer) {
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].Close(); err != nil {
			telemetry.WarnErr(context.Background(),
				"deploy: close agent part failed", err)
		}
	}
}

// liveProducer resolves a dep ref to the Go type of an already-built value.
// BindAgent uses it for agents assembled after the deployment build, where the
// plan is no longer in scope; the type is the value's dynamic type, which is
// exactly what a consumer's type assertion sees.
func liveProducer(result *Result) func(resource.Ref) (reflect.Type, error) {
	return func(ref resource.Ref) (reflect.Type, error) {
		name := ref.ResourceName()
		if ext, ok := result.externals[name]; ok {
			if _, hasItem := ref.ItemName(); hasItem {
				return nil, errdefs.Validationf(
					"external %q does not support item references", name)
			}
			return ext.Type, nil
		}
		value, ok := result.values[name]
		if !ok {
			return nil, errdefs.NotFoundf(
				"ref %q matches no resource and no external", string(ref))
		}
		if item, hasItem := ref.ItemName(); hasItem {
			resolver, ok := value.(resource.ItemResolver)
			if !ok {
				return nil, errdefs.Validationf(
					"resource %q does not expose items (ref %q)", name, string(ref))
			}
			itemValue, found := resolver.ResolveItem(item)
			if !found {
				return nil, errdefs.Validationf(
					"resource %q has no item %q", name, item)
			}
			return reflect.TypeOf(itemValue), nil
		}
		return reflect.TypeOf(value), nil
	}
}

// resolveDeps maps each declared dep to the built value, resolving
// "resource/item" refs through the container's [resource.ItemResolver].
func resolveDeps(
	values map[string]any,
	externals map[string]ExternalResource,
	deps resource.Deps,
) (map[string]any, error) {
	resolved := make(map[string]any, len(deps))
	for name, ref := range deps {
		refName := ref.ResourceName()
		value, ok := values[refName]
		if !ok {
			ext, external := externals[refName]
			if !external {
				return nil, errdefs.Validationf(
					"dep %q references unbuilt resource %q", name, refName)
			}
			if _, hasItem := ref.ItemName(); hasItem {
				return nil, errdefs.Validationf(
					"dep %q: external resource %q does not support item references",
					name, refName)
			}
			value = ext.Value
		}
		if _, isExternal := externals[refName]; isExternal {
			resolved[name] = value
			continue
		}
		if item, hasItem := ref.ItemName(); hasItem {
			resolver, ok := value.(resource.ItemResolver)
			if !ok {
				return nil, errdefs.Validationf(
					"dep %q: resource %q does not expose items", name, refName)
			}
			itemValue, ok := resolver.ResolveItem(item)
			if !ok {
				return nil, errdefs.Validationf(
					"dep %q: resource %q has no item %q", name, refName, item)
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
	values    map[string]any
	order     []string
	agents    map[string]*agent.Agent
	detached  map[string]struct{}
	externals map[string]ExternalResource
	// resolver and secrets carry the effective expansion state from
	// Build into Wire, so agent engine/hook settings expand with the
	// same schemes (including ${secret:...}) and typed Secrets resolve.
	resolver *resource.ReferenceResolver
	secrets  *resource.SecretResolver
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

// Resolver returns the effective settings reference resolver assembled
// by Build. Callers that construct values after the build (for example
// a runtime registering dynamic agents) pass it back into BindAgent so
// dynamic settings expand exactly like deployment-time settings.
func (r *Result) Resolver() *resource.ReferenceResolver {
	if r == nil {
		return nil
	}
	return r.resolver
}

// Secrets returns the effective secret resolver assembled by Build, or
// nil when the deployment declared no secret stores.
func (r *Result) Secrets() *resource.SecretResolver {
	if r == nil {
		return nil
	}
	return r.secrets
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
		if ptr.IsNil(closer) {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
