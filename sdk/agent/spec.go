package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DepSpec declares one named dependency an engine kind requires at
// assembly time. Names live in a single string key space so specs
// round-trip through YAML / JSON and dashboards without any
// Go-type-specific knowledge; Type is a documentation-and-validation
// hint naming the expected contract (e.g. "inference.Runtime",
// "tool.Catalog").
type DepSpec struct {
	// Name is the assembly-time key the host binds a value to
	// (e.g. "llm", "tools").
	Name string `json:"name" yaml:"name"`

	// Type names the contract the bound value must satisfy. It is
	// informational at this layer — factories perform the concrete
	// type assertion inside New and surface mismatches as
	// errdefs.Validation.
	Type string `json:"type" yaml:"type"`

	// Required reports whether assembly must fail when the dep is
	// absent. Optional deps bind as nil.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// EngineSpec is the static, serialisable declaration of one engine
// kind: what it is called, what optional capabilities it claims, and
// which named dependencies assembly must supply. It replaces the
// runtime interface-probing (Describer / wrapper chains) with a
// value the host can read before any engine instance exists.
type EngineSpec struct {
	// Kind is the registry key for this engine family
	// (e.g. "graph", "script", "inline").
	Kind string `json:"kind" yaml:"kind"`

	// Capabilities are the engine kind's claimed optional features.
	// Claims are made once, at registration — not probed per
	// instance.
	Capabilities Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`

	// Deps lists the named dependencies [Factory.New] expects in
	// [Config.Deps].
	Deps []DepSpec `json:"deps,omitempty" yaml:"deps,omitempty"`
}

// Config carries the resolved inputs a [Factory] needs to build one
// engine instance. Values in Deps are keyed by [DepSpec.Name]; the
// factory type-asserts each entry to the contract its DepSpec
// declares and returns errdefs.Validation on mismatch, so wiring
// mistakes surface at assembly time rather than mid-run.
type Config struct {
	// Deps holds assembled dependency values keyed by DepSpec.Name.
	// May be nil when the engine kind declares no deps.
	Deps map[string]any

	// Settings carries engine-kind-specific configuration (graph
	// definition path, script source, tuning knobs) as plain
	// YAML-decoded data. It is opaque to everything except the
	// factory, which decodes and strictly validates it inside New.
	Settings map[string]any
}

// Dep returns the bound value for name, or false when absent.
func (c Config) Dep(name string) (any, bool) {
	if c.Deps == nil {
		return nil, false
	}
	v, ok := c.Deps[name]
	return v, ok
}

// Setting returns the engine-kind-specific setting for name, or
// false when absent.
func (c Config) Setting(name string) (any, bool) {
	if c.Settings == nil {
		return nil, false
	}
	v, ok := c.Settings[name]
	return v, ok
}

// Factory builds engines of one kind. A factory is the only place
// that knows how to turn serialisable spec + resolved deps into a
// runnable [Engine]; hosts and config loaders depend on the
// interface, never on concrete engine types.
//
// New MUST validate cfg against Spec().Deps and fail fast with
// errdefs.Validation / errdefs.NotFound on missing or mistyped
// entries — that contract is what lets assembly-time tooling trust
// the spec without executing the engine.
type Factory interface {
	// Spec returns the static declaration for this engine kind.
	Spec() EngineSpec

	// New builds an engine instance from resolved configuration.
	New(ctx context.Context, cfg Config) (Engine, error)
}

// Registry maps engine kinds to their factories. It is the static
// home for "which engine kinds exist and what do they need" — the
// lookup that capability probing used to answer at run time.
//
// Registries are safe for concurrent use; Register is expected at
// process assembly (init / main), Lookup at run time.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty engine registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds f under its Spec().Kind. It fails when f is nil, the
// kind is empty, or the kind is already registered — duplicate kinds
// are configuration bugs, not last-one-wins.
func (r *Registry) Register(f Factory) error {
	if f == nil {
		return errdefs.Validationf("agent.Registry: nil factory")
	}
	kind := f.Spec().Kind
	if kind == "" {
		return errdefs.Validationf("agent.Registry: factory %T has empty kind", f)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[kind]; dup {
		return errdefs.Validationf("agent.Registry: engine kind %q already registered", kind)
	}
	r.factories[kind] = f
	return nil
}

// MustRegister is Register that panics on error — for init-time
// registration where a failure is a programming bug.
func (r *Registry) MustRegister(f Factory) {
	if err := r.Register(f); err != nil {
		panic(err)
	}
}

// Lookup returns the factory registered under kind.
func (r *Registry) Lookup(kind string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[kind]
	return f, ok
}

// Specs returns every registered engine spec, sorted by kind for
// stable display (admin UIs, diagnostics dumps).
func (r *Registry) Specs() []EngineSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EngineSpec, 0, len(r.factories))
	for _, f := range r.factories {
		out = append(out, f.Spec())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// String makes Registry printable in diagnostics.
func (r *Registry) String() string {
	specs := r.Specs()
	kinds := make([]string, len(specs))
	for i, s := range specs {
		kinds[i] = s.Kind
	}
	return fmt.Sprintf("agent.Registry%v", kinds)
}
