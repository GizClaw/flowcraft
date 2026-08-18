package resource

import (
	"context"
	"fmt"
	"sync"
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

// TestRegistryConcurrentAccess runs concurrent Register / Lookup / Specs
// to prove the registry is safe under -race. Registration is host-owned,
// so distinct (kind, impl) keys are used to keep the writes legal.
func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	const writers = 8
	const perWriter = 64
	const readers = 8

	// Seed a base set every reader keeps looking up while writers add
	// new keys concurrently. New keys are asserted after the writers
	// finish; lookups of not-yet-registered keys are expected misses.
	for i := 0; i < 16; i++ {
		r.MustRegister(fakeFactory{
			spec: Spec{Kind: Kind(fmt.Sprintf("base.%d", i)), Impl: "impl"},
		})
	}

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				kind := Kind(fmt.Sprintf("kind.%d.%d", w, i))
				if err := r.Register(fakeFactory{
					spec: Spec{Kind: kind, Impl: "impl"},
				}); err != nil {
					t.Errorf("Register(%s): %v", kind, err)
					return
				}
			}
		}(w)
	}
	for rIdx := 0; rIdx < readers; rIdx++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter*2; i++ {
				base := Kind(fmt.Sprintf("base.%d", i%16))
				if _, ok := r.Lookup(base, "impl"); !ok {
					t.Errorf("Lookup(%s) = not found", base)
					return
				}
				_ = r.Specs()
			}
		}(rIdx)
	}
	wg.Wait()
	if got := len(r.Specs()); got != writers*perWriter+16 {
		t.Fatalf("Specs() = %d entries, want %d", got, writers*perWriter+16)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			kind := Kind(fmt.Sprintf("kind.%d.%d", w, i))
			if _, ok := r.Lookup(kind, "impl"); !ok {
				t.Fatalf("Lookup(%s) = not found after writers finished", kind)
			}
		}
	}
}
