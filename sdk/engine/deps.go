package engine

import (
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Dependencies is a typed dependency-injection container that an engine
// host (typically the agent runtime) populates at build time and that
// engine implementations consume at run time.
//
// The container is keyed by an opaque any (use a typed key constant
// per dependency, never a bare string) to keep the lookup explicit and
// to discourage stringly-typed coupling. Concurrent reads after Build
// are safe; mutations through Set/Remove acquire a write lock so a
// host can adjust the container at any time.
//
// This replaces graph.Dependencies / workflow.ToolDeps and intentionally
// has no engine-specific knowledge.
type Dependencies struct {
	mu    sync.RWMutex
	items map[any]any
}

// NewDependencies creates an empty dependency container.
func NewDependencies() *Dependencies {
	return &Dependencies{items: make(map[any]any)}
}

// Set stores a dependency under the given key. Overwrites any existing
// value for the same key.
func (d *Dependencies) Set(key, value any) {
	d.mu.Lock()
	if d.items == nil {
		d.items = make(map[any]any)
	}
	d.items[key] = value
	d.mu.Unlock()
}

// Get returns the dependency for the given key, untyped.
func (d *Dependencies) Get(key any) (any, bool) {
	if d == nil {
		return nil, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.items[key]
	return v, ok
}

// Remove deletes a dependency. Missing keys are a no-op.
func (d *Dependencies) Remove(key any) {
	d.mu.Lock()
	delete(d.items, key)
	d.mu.Unlock()
}

// Has reports whether the given key is present.
func (d *Dependencies) Has(key any) bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.items[key]
	return ok
}

// GetDep is a generic helper that retrieves a typed dependency. It
// returns an error when the key is missing or when the stored value is
// not assignable to T, so callers can surface configuration mistakes
// early instead of failing with a nil-pointer panic deep inside an
// engine.
func GetDep[T any](d *Dependencies, key any) (T, error) {
	var zero T
	if d == nil {
		// nil container is a host-side wiring bug — the run was
		// started without the dependencies it needs. Classify as
		// Internal so callers / observability can distinguish "you
		// forgot to wire deps" from "this specific dep is missing"
		// (NotFound, below).
		return zero, errdefs.Internalf("engine.GetDep: nil dependencies (looking up %v)", key)
	}
	raw, ok := d.Get(key)
	if !ok {
		return zero, errdefs.NotFoundf("engine.GetDep: dependency %v not found", key)
	}
	v, ok := raw.(T)
	if !ok {
		return zero, errdefs.Validationf("engine.GetDep: dependency %v has type %T, want %T", key, raw, zero)
	}
	return v, nil
}

// MustGetDep is like [GetDep] but panics on error. Use it only inside
// engine internals where a missing dependency is a programming bug
// (e.g. a node referenced a dep that the host did not register).
func MustGetDep[T any](d *Dependencies, key any) T {
	v, err := GetDep[T](d, key)
	if err != nil {
		panic(err)
	}
	return v
}
