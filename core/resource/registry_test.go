package resource

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

type fakeFactory struct{ spec Spec }

func (f fakeFactory) Spec() Spec { return f.spec }
func (fakeFactory) New(context.Context, Input) (any, error) {
	return "value", nil
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	f := fakeFactory{spec: Spec{Kind: "workspace.Registry", Impl: "local"}}
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("workspace.Registry", "local")
	if !ok || got.Spec().Kind != f.Spec().Kind || got.Spec().Impl != f.Spec().Impl {
		t.Fatalf("Lookup = (%v, %v), want registered factory", got, ok)
	}
	if _, ok := r.Lookup("workspace.Registry", "mem"); ok {
		t.Fatal("Lookup found unregistered impl")
	}
}

func TestRegistryRejectsDuplicateAndInvalid(t *testing.T) {
	r := NewRegistry()
	f := fakeFactory{spec: Spec{Kind: "event.Bus", Impl: "memory"}}
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(f); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate Register error = %v, want conflict", err)
	}
	if err := r.Register(fakeFactory{spec: Spec{Impl: "x"}}); !errdefs.IsValidation(err) {
		t.Fatalf("invalid spec error = %v, want validation", err)
	}
}

func TestRegistrySpecs(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(fakeFactory{spec: Spec{Kind: "event.Bus", Impl: "memory"}})
	r.MustRegister(fakeFactory{spec: Spec{Kind: "tool.Assembly", Impl: "yaml"}})
	if got := len(r.Specs()); got != 2 {
		t.Fatalf("Specs() = %d entries, want 2", got)
	}
}
