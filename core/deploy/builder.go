package deploy

import (
	"context"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// Builder constructs a deployment's resources from a [Document] using
// an explicit [resource.Registry]. The registry is owned by the caller;
// Builder never touches global state.
type Builder struct {
	registry *resource.Registry
	loader   *resource.Loader
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithLoader supplies the deployment-level loader used to materialize
// settings subtrees that are {"file":…} / {"embed":…} references, and
// passed to factories for their own source resolution.
func WithLoader(loader *resource.Loader) BuilderOption {
	return func(b *Builder) { b.loader = loader }
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
	for _, name := range order {
		res := doc.Resources[name]
		factory, ok := b.registry.Lookup(res.Kind, res.Impl)
		if !ok {
			_ = closeAll(values, order)
			return nil, errdefs.Validationf(
				"deploy: resource %q: no factory for %s/%s",
				name, res.Kind, res.Impl)
		}
		if err := validateDeps(factory, res.Deps); err != nil {
			_ = closeAll(values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		settings := res.Settings
		if b.loader != nil {
			settings, err = resolveSettings(ctx, b.loader, settings)
			if err != nil {
				_ = closeAll(values, order)
				return nil, errdefs.Validationf(
					"deploy: resource %q: %v", name, err)
			}
		}
		deps, err := resolveDeps(values, res.Deps)
		if err != nil {
			_ = closeAll(values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		value, err := factory.New(ctx, resource.Input{
			Settings: settings,
			Deps:     deps,
			Loader:   b.loader,
		})
		if err != nil {
			_ = closeAll(values, order)
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
		_ = result.Close()
		return nil, err
	}
	return result, nil
}

// bindAgents constructs every [agent.Agent] from its Definition: the
// engine from the registry (kind = EngineRef.Kind / Impl), then each
// hook under "hook.<slot>". Hooks are wired (attached) before being
// recorded on the agent.
func (b *Builder) bindAgents(ctx context.Context, result *Result, doc Document) error {
	for name, def := range doc.Agents {
		engineFactory, ok := b.registry.Lookup(def.Engine.Kind, def.Engine.Impl)
		if !ok {
			return errdefs.Validationf(
				"deploy: agent %q: no factory for engine %s/%s",
				name, def.Engine.Kind, def.Engine.Impl)
		}
		if err := validateDeps(engineFactory, def.Engine.Deps); err != nil {
			return errdefs.Validationf("deploy: agent %q engine: %v", name, err)
		}
		engineDeps, err := resolveDeps(result.values, def.Engine.Deps)
		if err != nil {
			return errdefs.Validationf("deploy: agent %q: %v", name, err)
		}
		engine, err := engineFactory.New(ctx, resource.Input{
			Settings: def.Engine.Settings,
			Deps:     engineDeps,
		})
		if err != nil {
			return errdefs.Validationf("deploy: agent %q engine: %v", name, err)
		}
		engineContract, ok := engine.(agent.Engine)
		if !ok {
			return errdefs.Validationf(
				"deploy: agent %q: engine factory returned %T, want agent.Engine",
				name, engine)
		}

		prepare, err := buildHookList[agent.Preparer](
			b, result, ctx, name, agent.HookSlotPreparer, def.Prepare)
		if err != nil {
			return err
		}
		observe, err := buildHookList[agent.Observer](
			b, result, ctx, name, agent.HookSlotObserver, def.Observe)
		if err != nil {
			return err
		}
		referees, err := buildHookList[agent.Referee](
			b, result, ctx, name, agent.HookSlotReferee, def.Referees)
		if err != nil {
			return err
		}
		commit, err := buildHookList[agent.Committer](
			b, result, ctx, name, agent.HookSlotCommitter, def.Commit)
		if err != nil {
			return err
		}

		result.agents[name] = &agent.Agent{
			ID:       name,
			Card:     def.Card,
			Tools:    def.Tools,
			Policy:   def.Policy,
			Engine:   engineContract,
			Prepare:  prepare,
			Observe:  observe,
			Referees: referees,
			Commit:   commit,
		}
	}
	return nil
}

// buildHookList constructs every hook in one slot, type-asserting the
// factory values to T (agent.Preparer / Observer / Referee /
// Committer) and wiring [resource.Wireable] values before recording
// them on the agent.
func buildHookList[T any](
	b *Builder,
	result *Result,
	ctx context.Context,
	name, slot string,
	entries []agent.Hook,
) ([]T, error) {
	var values []T
	for i, entry := range entries {
		kind := resource.Kind("hook." + slot)
		factory, ok := b.registry.Lookup(kind, entry.Type)
		if !ok {
			return nil, errdefs.Validationf(
				"deploy: agent %q: no factory for hook %s/%s",
				name, kind, entry.Type)
		}
		if err := validateDeps(factory, entry.Deps); err != nil {
			return nil, errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err)
		}
		deps, err := resolveDeps(result.values, entry.Deps)
		if err != nil {
			return nil, errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err)
		}
		value, err := factory.New(ctx, resource.Input{
			Settings: entry.Settings,
			Deps:     deps,
		})
		if err != nil {
			return nil, errdefs.Validationf(
				"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err)
		}
		typed, ok := value.(T)
		if !ok {
			return nil, errdefs.Validationf(
				"deploy: agent %q: hook %s[%d] factory returned %T, want %T",
				name, slot, i, value, *new(T))
		}
		if w, ok := value.(resource.Wireable); ok {
			if err := w.Wire(ctx); err != nil {
				return nil, errdefs.Validationf(
					"deploy: agent %q: wire hook %s[%d]: %v", name, slot, i, err)
			}
		}
		values = append(values, typed)
	}
	return values, nil
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
	values map[string]any
	order  []string
	agents map[string]*agent.Agent
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

// Close closes every io.Closer value in reverse construction order.
func (r *Result) Close() error {
	return closeAll(r.values, r.order)
}

func closeAll(values map[string]any, order []string) error {
	var first error
	for i := len(order) - 1; i >= 0; i-- {
		value, ok := values[order[i]]
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
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
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
