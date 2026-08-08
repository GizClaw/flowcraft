package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// SourceFunc resolves one dep reference into a value the HOST owns —
// a closure over an instance built outside a deployment document.
// Resolution happens once, at build time, and the returned value is
// never closed by the assembly result: a source is borrowed, not owned.
type SourceFunc func(ctx context.Context, ref string) (any, error)

// DepSpec declares one named dependency accepted by a build factory.
// Names live in a single string key space so specs round-trip through
// YAML / JSON without Go-type knowledge; Type is a
// documentation-and-validation hint naming the expected contract
// (e.g. "inference.Runtime", "workspace.Workspace").
type DepSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// Spec is the static declaration of one build factory: the unique
// (Kind, Impl) registry key, the named dependencies [Factory.New]
// expects in [Input.Deps], and the item type container factories
// expose through [ItemResolver]. Spec is what assembly tooling reads
// before any instance exists.
type Spec struct {
	Kind     string    `json:"kind"`
	Impl     string    `json:"impl,omitempty"`
	Deps     []DepSpec `json:"deps,omitempty"`
	ItemType string    `json:"item_type,omitempty"`
}

// Clone returns a defensive copy of the spec: the returned value
// shares no backing array with the receiver.
func (s Spec) Clone() Spec {
	s.Deps = append([]DepSpec(nil), s.Deps...)
	return s
}

// Validate checks the static invariants every factory spec must
// satisfy. A spec with an empty kind is unregistrable; Impl may be
// empty for a kind with a single implementation (an engine kind), and
// every declared dependency must have a name and a type; names must
// not repeat.
func (s Spec) Validate() error {
	if s.Kind == "" {
		return errdefs.Validationf("config factory spec: kind is empty")
	}
	seen := make(map[string]struct{}, len(s.Deps))
	for i, dep := range s.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"config factory spec %s/%s: deps[%d].name is empty",
				s.Kind, s.Impl, i)
		}
		if dep.Type == "" {
			return errdefs.Validationf(
				"config factory spec %s/%s: dep %q type is empty",
				s.Kind, s.Impl, dep.Name)
		}
		if _, dup := seen[dep.Name]; dup {
			return errdefs.Validationf(
				"config factory spec %s/%s: duplicate dep %q",
				s.Kind, s.Impl, dep.Name)
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

// Factory builds one value from an [Input] of raw settings and
// already-built dependencies. Factories decode and strictly validate
// their own settings inside New, so a typo fails the build instead of
// silently dropping policy. The returned value's type is declared by
// the Spec; the assembly engine asserts it where needed. A value
// implementing io.Closer is closed by the assembly result in reverse
// construction order.
type Factory interface {
	// Spec returns the static declaration for this factory.
	Spec() Spec

	// New builds one value from resolved settings and dependencies.
	New(ctx context.Context, in Input) (any, error)
}

// ItemResolver is implemented by container factories that hold named
// items — a workspace registry's workspaces, a sandbox registry's
// runners. The scalar dep form "resource/item" resolves through it.
//
// A factory may bind whole and also expose deliberately typed items.
// For example, an inference assembly binds whole to graph engines and
// exposes its exact runtime as "runtime". An undeclared item name is
// a build error.
type ItemResolver interface {
	ResolveItem(ref string) (any, bool)
}
