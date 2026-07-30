package config

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	yamlv3 "gopkg.in/yaml.v3"
)

// SourceFunc resolves one dep reference into an assembled value —
// a closure over an inference registry, a tool catalog, or any
// other application-side wiring YAML cannot express. Resolution
// happens once, at Build time.
type SourceFunc func(ctx context.Context, ref string) (any, error)

// HookFactory builds one lifecycle hook from its opaque settings
// node. Factories MUST decode strictly (KnownFields-style: unknown
// fields are errors) so a typo in YAML fails the build rather than
// silently dropping policy.
type HookFactory func(ctx context.Context, settings *yamlv3.Node) (agent.Hook, error)

// BeforeFactory builds the [agent.BeforeExecute] seed hook.
type BeforeFactory func(ctx context.Context, settings *yamlv3.Node) (agent.BeforeExecute, error)

// AfterFactory builds one [agent.AfterExecute] decision hook.
type AfterFactory func(ctx context.Context, settings *yamlv3.Node) (agent.AfterExecute, error)

// Instance is one assembled, runnable agent: identity + engine +
// the per-call options the document declared. Execute appends the
// document-derived options before the caller's own, so a call site
// can still override policy per invocation.
type Instance struct {
	Agent  agent.Agent
	Engine agent.Engine

	opts []agent.ExecuteOption
}

// Execute runs one turn of this instance. Caller options are
// appended AFTER the document-derived ones and therefore win on
// conflict (the harness's caller-supplied-wins rule).
func (i *Instance) Execute(ctx context.Context, req agent.Request, opts ...agent.ExecuteOption) (*agent.Result, error) {
	all := make([]agent.ExecuteOption, 0, len(i.opts)+len(opts))
	all = append(all, i.opts...)
	all = append(all, opts...)
	return agent.Execute(ctx, i.Agent, i.Engine, req, all...)
}

// Builder turns validated Documents into Instances. It binds:
//
//   - an [agent.Registry] of engine factories (engine.kind lookup);
//   - named dep SOURCES (how "inference.profile"/"kimi-k2" becomes
//     a value);
//   - named hook / before / after factories (how lifecycle entries
//     become implementations).
//
// No global registration: two Builders in one process stay
// independent.
type Builder struct {
	engines *agent.Registry
	sources map[string]SourceFunc
	hooks   map[string]HookFactory
	befores map[string]BeforeFactory
	afters  map[string]AfterFactory
}

// NewBuilder returns a Builder over the given engine registry with
// the built-in after factories registered. A nil registry panics —
// a Builder without engine kinds cannot assemble anything.
func NewBuilder(engines *agent.Registry) *Builder {
	if engines == nil {
		panic("config.NewBuilder: engine registry is nil")
	}
	b := &Builder{
		engines: engines,
		sources: make(map[string]SourceFunc),
		hooks:   make(map[string]HookFactory),
		befores: make(map[string]BeforeFactory),
		afters:  make(map[string]AfterFactory),
	}
	b.registerBuiltins()
	return b
}

// RegisterSource adds (or replaces) the resolver for a dep source
// name. Empty names and nil funcs are programming bugs.
func (b *Builder) RegisterSource(name string, fn SourceFunc) {
	if name == "" {
		panic("config.RegisterSource: name is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("config.RegisterSource: source %q is nil", name))
	}
	b.sources[name] = fn
}

// RegisterHook adds (or replaces) a lifecycle hook factory.
func (b *Builder) RegisterHook(typ string, fn HookFactory) {
	if typ == "" {
		panic("config.RegisterHook: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("config.RegisterHook: factory for type %q is nil", typ))
	}
	b.hooks[typ] = fn
}

// RegisterBefore adds (or replaces) a BeforeExecute factory.
func (b *Builder) RegisterBefore(typ string, fn BeforeFactory) {
	if typ == "" {
		panic("config.RegisterBefore: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("config.RegisterBefore: factory for type %q is nil", typ))
	}
	b.befores[typ] = fn
}

// RegisterAfter adds (or replaces) an AfterExecute factory.
func (b *Builder) RegisterAfter(typ string, fn AfterFactory) {
	if typ == "" {
		panic("config.RegisterAfter: type is empty")
	}
	if fn == nil {
		panic(fmt.Sprintf("config.RegisterAfter: factory for type %q is nil", typ))
	}
	b.afters[typ] = fn
}

// Build assembles every agent in doc. Agent ids are processed in
// sorted order so failures report deterministically.
func (b *Builder) Build(ctx context.Context, doc Document) (map[string]*Instance, error) {
	ids := make([]string, 0, len(doc.Agents))
	for id := range doc.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make(map[string]*Instance, len(doc.Agents))
	for _, id := range ids {
		inst, err := b.buildOne(ctx, id, doc.Agents[id])
		if err != nil {
			return nil, err
		}
		out[id] = inst
	}
	return out, nil
}

func (b *Builder) buildOne(ctx context.Context, id string, entry AgentEntry) (*Instance, error) {
	factory, ok := b.engines.Lookup(entry.Engine.Kind)
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"agent config agents[%q]: engine kind %q is not registered", id, entry.Engine.Kind))
	}

	spec := factory.Spec()
	deps, err := b.resolveDeps(ctx, id, spec, entry.Deps)
	if err != nil {
		return nil, err
	}

	eng, err := factory.New(ctx, agent.Config{
		Deps:     deps,
		Settings: entry.Engine.Settings,
	})
	if err != nil {
		return nil, fmt.Errorf("agent config agents[%q]: engine %q build: %w", id, entry.Engine.Kind, err)
	}

	inst := &Instance{
		Agent: agent.Agent{
			ID: id,
			Card: agent.AgentCard{
				Name:        entry.Card.Name,
				Description: entry.Card.Description,
			},
			Tools: entry.Tools,
		},
		Engine: eng,
	}

	if entry.Before != nil {
		before, err := b.buildBefore(ctx, id, *entry.Before)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithBeforeExecute(before))
	}
	for i, h := range entry.Hooks {
		hook, err := b.buildHook(ctx, id, i, h)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithHook(hook))
	}
	for i, h := range entry.After {
		after, err := b.buildAfter(ctx, id, i, h)
		if err != nil {
			return nil, err
		}
		inst.opts = append(inst.opts, agent.WithAfterExecute(after))
	}
	if entry.Policy.MaxRevise > 0 {
		inst.opts = append(inst.opts, agent.WithMaxRevise(entry.Policy.MaxRevise))
	}
	if len(entry.Policy.ArtifactChannels) > 0 {
		inst.opts = append(inst.opts, agent.WithArtifactChannels(entry.Policy.ArtifactChannels...))
	}
	return inst, nil
}

// resolveDeps binds every YAML dep entry through its named source
// and validates the result against the factory's DepSpec list:
// unknown dep names and missing required deps are build errors.
func (b *Builder) resolveDeps(ctx context.Context, id string, spec agent.EngineSpec, refs map[string]DepRef) (map[string]any, error) {
	declared := make(map[string]agent.DepSpec, len(spec.Deps))
	for _, ds := range spec.Deps {
		declared[ds.Name] = ds
	}

	deps := make(map[string]any, len(refs))
	for name, ref := range refs {
		ds, ok := declared[name]
		if !ok {
			return nil, errdefs.Validation(fmt.Errorf(
				"agent config agents[%q].deps[%q]: engine kind %q declares no such dep (declared: %v)",
				id, name, spec.Kind, depNames(spec.Deps)))
		}
		src, ok := b.sources[ref.Source]
		if !ok {
			return nil, errdefs.NotFound(fmt.Errorf(
				"agent config agents[%q].deps[%q]: source %q is not registered", id, name, ref.Source))
		}
		v, err := src(ctx, ref.Ref)
		if err != nil {
			return nil, fmt.Errorf(
				"agent config agents[%q].deps[%q] (%s, ref %q): %w",
				id, name, ds.Type, ref.Ref, err)
		}
		deps[name] = v
	}

	for _, ds := range spec.Deps {
		if ds.Required {
			if _, ok := deps[ds.Name]; !ok {
				return nil, errdefs.NotFound(fmt.Errorf(
					"agent config agents[%q]: required dep %q (%s) is not bound", id, ds.Name, ds.Type))
			}
		}
	}
	return deps, nil
}

func (b *Builder) buildHook(ctx context.Context, id string, idx int, h HookEntry) (agent.Hook, error) {
	fn, ok := b.hooks[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"agent config agents[%q].hooks[%d]: hook type %q is not registered", id, idx, h.Type))
	}
	hook, err := fn(ctx, h.Settings.Node())
	if err != nil {
		return nil, fmt.Errorf("agent config agents[%q].hooks[%d] (%q): %w", id, idx, h.Type, err)
	}
	if hook == nil {
		return nil, errdefs.Internalf("agent config agents[%q].hooks[%d]: factory for %q returned nil", id, idx, h.Type)
	}
	return hook, nil
}

func (b *Builder) buildBefore(ctx context.Context, id string, h HookEntry) (agent.BeforeExecute, error) {
	fn, ok := b.befores[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"agent config agents[%q].before: type %q is not registered", id, h.Type))
	}
	before, err := fn(ctx, h.Settings.Node())
	if err != nil {
		return nil, fmt.Errorf("agent config agents[%q].before (%q): %w", id, h.Type, err)
	}
	if before == nil {
		return nil, errdefs.Internalf("agent config agents[%q].before: factory for %q returned nil", id, h.Type)
	}
	return before, nil
}

func (b *Builder) buildAfter(ctx context.Context, id string, idx int, h HookEntry) (agent.AfterExecute, error) {
	fn, ok := b.afters[h.Type]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"agent config agents[%q].after[%d]: type %q is not registered", id, idx, h.Type))
	}
	after, err := fn(ctx, h.Settings.Node())
	if err != nil {
		return nil, fmt.Errorf("agent config agents[%q].after[%d] (%q): %w", id, idx, h.Type, err)
	}
	if after == nil {
		return nil, errdefs.Internalf("agent config agents[%q].after[%d]: factory for %q returned nil", id, idx, h.Type)
	}
	return after, nil
}

func depNames(specs []agent.DepSpec) []string {
	out := make([]string, len(specs))
	for i, ds := range specs {
		out[i] = ds.Name
	}
	return out
}
