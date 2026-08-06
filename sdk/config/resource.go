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

// ResourceDepSpec declares one named dependency accepted by a resource
// factory.
type ResourceDepSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// ResourceSpec is the static declaration for one resource factory.
// Kind and Impl form its registry key. ItemType is non-empty when the
// resource is a container whose named items can be resolved.
type ResourceSpec struct {
	Kind     string            `json:"kind"`
	Impl     string            `json:"impl"`
	Deps     []ResourceDepSpec `json:"deps,omitempty"`
	ItemType string            `json:"item_type,omitempty"`
}

// Clone returns a defensive copy of the spec: the returned value shares
// no backing array with the receiver.
func (s ResourceSpec) Clone() ResourceSpec {
	s.Deps = append([]ResourceDepSpec(nil), s.Deps...)
	return s
}

// Validate checks the static invariants every resource factory spec
// must satisfy. A spec with an empty kind or impl is unregistrable, and
// every declared dependency must have a name and a type; names must not
// repeat.
func (s ResourceSpec) Validate() error {
	if s.Kind == "" {
		return errdefs.Validationf("config resource spec: kind is empty")
	}
	if s.Impl == "" {
		return errdefs.Validationf(
			"config resource spec %q: impl is empty", s.Kind)
	}
	seen := make(map[string]struct{}, len(s.Deps))
	for i, dep := range s.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"config resource spec %s/%s: deps[%d].name is empty",
				s.Kind, s.Impl, i)
		}
		if dep.Type == "" {
			return errdefs.Validationf(
				"config resource spec %s/%s: dep %q type is empty",
				s.Kind, s.Impl, dep.Name)
		}
		if _, dup := seen[dep.Name]; dup {
			return errdefs.Validationf(
				"config resource spec %s/%s: duplicate dep %q",
				s.Kind, s.Impl, dep.Name)
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

// ResourceFactory declares and builds one shared resource.
//
// A returned value implementing io.Closer is closed by the assembly
// result in reverse construction order. A constructor that returns
// something it does NOT want closed should wrap it.
type ResourceFactory interface {
	Spec() ResourceSpec
	New(ctx context.Context, in Input) (any, error)
}

// ItemResolver is implemented by container resources that hold named
// items — a workspace registry's workspaces, a sandbox registry's
// runners. The scalar dep form "resource/item" resolves through it.
//
// A resource may bind whole and also expose deliberately typed items.
// For example, an inference assembly binds whole to graph engines and
// exposes its exact runtime as "runtime". An undeclared item name is a
// build error.
type ItemResolver interface {
	ResolveItem(ref string) (any, bool)
}
