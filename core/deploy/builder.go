package deploy

import (
	"context"
	"io"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Builder constructs a deployment's resources from a [Document] using
// an explicit [resource.Registry]. The registry is owned by the caller;
// Builder never touches global state.
type Builder struct {
	registry *resource.Registry
}

// NewBuilder returns a Builder over registry. A nil registry yields an
// empty one.
func NewBuilder(registry *resource.Registry) *Builder {
	if registry == nil {
		registry = resource.NewRegistry()
	}
	return &Builder{registry: registry}
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
			closeAll(values, order)
			return nil, errdefs.Validationf(
				"deploy: resource %q: no factory for %s/%s",
				name, res.Kind, res.Impl)
		}
		deps, err := resolveDeps(values, res.Deps)
		if err != nil {
			closeAll(values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		value, err := factory.New(ctx, resource.Input{
			Settings: res.Settings,
			Deps:     deps,
		})
		if err != nil {
			closeAll(values, order)
			return nil, errdefs.Validationf("deploy: resource %q: %v", name, err)
		}
		values[name] = value
	}

	return &Result{
		values: values,
		order:  order,
		agents: make(map[string]*BoundAgent, len(doc.Agents)),
	}, nil
}

// Wire runs the post-build wiring phase: resource values implementing
// [resource.Wireable] attach themselves (observers to buses, hooks to
// streams), then every agent is bound from its engine and hooks. Wire
// never participates in the construction DAG, so observed values never
// depend on their observers.
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
	return b.bindAgents(ctx, result, doc)
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

// bindAgents constructs every agent: the engine from the registry
// (kind = agent.Engine.Kind), then each hook under "hook.<slot>".
// Hooks are wired (attached) before being recorded on the agent.
func (b *Builder) bindAgents(ctx context.Context, result *Result, doc Document) error {
	for name, def := range doc.Agents {
		engineFactory, ok := b.registry.Lookup(def.Engine.Kind, def.Engine.Impl)
		if !ok {
			return errdefs.Validationf(
				"deploy: agent %q: no factory for engine %s/%s",
				name, def.Engine.Kind, def.Engine.Impl)
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

		hooks := make(map[string][]any, len(def.Hooks))
		for slot, entries := range def.Hooks {
			for i, entry := range entries {
				kind := resource.Kind("hook." + slot)
				factory, ok := b.registry.Lookup(kind, entry.Type)
				if !ok {
					return errdefs.Validationf(
						"deploy: agent %q: no factory for hook %s/%s",
						name, kind, entry.Type)
				}
				deps, err := resolveDeps(result.values, entry.Deps)
				if err != nil {
					return errdefs.Validationf(
						"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err)
				}
				value, err := factory.New(ctx, resource.Input{
					Settings: entry.Settings,
					Deps:     deps,
				})
				if err != nil {
					return errdefs.Validationf(
						"deploy: agent %q: hook %s[%d]: %v", name, slot, i, err)
				}
				if w, ok := value.(resource.Wireable); ok {
					if err := w.Wire(ctx); err != nil {
						return errdefs.Validationf(
							"deploy: agent %q: wire hook %s[%d]: %v", name, slot, i, err)
					}
				}
				hooks[slot] = append(hooks[slot], value)
			}
		}

		result.agents[name] = &BoundAgent{
			Name:       name,
			Definition: def,
			Engine:     engine,
			Hooks:      hooks,
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
	agents map[string]*BoundAgent
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

// BoundAgent is a wired agent: the constructed engine and the attached
// lifecycle hooks, keyed by slot.
type BoundAgent struct {
	Name       string
	Definition agent.Definition
	Engine     any
	Hooks      map[string][]any
}

// Agent returns the bound agent registered under name.
func (r *Result) Agent(name string) (*BoundAgent, bool) {
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
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
