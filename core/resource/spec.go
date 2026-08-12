package resource

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// DepSpec declares one named dependency accepted by a build factory.
// Type names the expected contract (e.g. "sandbox.Runner") for
// documentation and validation; it is not resolved by this package.
type DepSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// Spec is the static declaration of one build factory: the unique
// (Kind, Impl) registry key and the named dependencies [Factory.New]
// expects in [Input.Deps].
type Spec struct {
	Kind     Kind      `json:"kind"`
	Impl     string    `json:"impl,omitempty"`
	Deps     []DepSpec `json:"deps,omitempty"`
	ItemType string    `json:"item_type,omitempty"`
}

// Clone returns a defensive copy: the returned spec shares no backing
// array with the receiver.
func (s Spec) Clone() Spec {
	s.Deps = append([]DepSpec(nil), s.Deps...)
	return s
}

// Validate checks the static invariants every factory spec must
// satisfy: a non-empty kind, named+typed deps, no duplicate names.
func (s Spec) Validate() error {
	if s.Kind == "" {
		return errdefs.Validationf("resource factory spec: kind is empty")
	}
	seen := make(map[string]struct{}, len(s.Deps))
	for i, dep := range s.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"resource factory spec %s/%s: deps[%d].name is empty",
				s.Kind, s.Impl, i)
		}
		if dep.Type == "" {
			return errdefs.Validationf(
				"resource factory spec %s/%s: dep %q type is empty",
				s.Kind, s.Impl, dep.Name)
		}
		if _, dup := seen[dep.Name]; dup {
			return errdefs.Validationf(
				"resource factory spec %s/%s: duplicate dep %q",
				s.Kind, s.Impl, dep.Name)
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

// Factory builds one resource value from an [Input] of raw settings
// and already-built dependencies. Factories decode and strictly
// validate their own settings inside New. A value implementing
// io.Closer is closed by the assembly layer in reverse construction
// order.
type Factory interface {
	// Spec returns the static declaration for this factory.
	Spec() Spec

	// New builds one value from resolved settings and dependencies.
	New(ctx context.Context, in Input) (any, error)
}

// ItemResolver is implemented by container resources that expose named
// items, making "resource/item" refs resolvable. A workspace registry
// exposing its workspaces, or a sandbox registry exposing its runners,
// are examples.
type ItemResolver interface {
	ResolveItem(item string) (any, bool)
}

// Wireable is implemented by resource values that need a post-build
// attachment step: observers attaching to buses, hooks subscribing to
// event streams, integrations registering into hosts. Wire runs after
// every resource is constructed and never participates in the
// construction DAG, so an observed value can never depend on its
// observer.
type Wireable interface {
	Wire(ctx context.Context) error
}

// Input is the universal factory input: the factory-owned settings
// subtree as raw JSON plus already-built dependencies keyed by the
// names used in the document.
type Input struct {
	Settings []byte
	Deps     map[string]any
}

// Dep returns the named dependency.
func (in Input) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
}
