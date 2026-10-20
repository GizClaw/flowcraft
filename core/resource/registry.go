package resource

import (
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Key is the unique (Kind, Impl) identity of a factory in a registry.
type Key struct {
	Kind Kind
	Impl string
}

// Registry maps resource kinds (+ implementations) to factories. Each
// resource module registers its factories explicitly; there is no
// global state.
//
// A registry holds [Entry] values, so every registration carries the Go type
// its factory returns. The assembly layer reads that type to check a whole
// deployment before constructing anything.
type Registry struct {
	mu      sync.RWMutex
	entries map[Key]Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[Key]Entry)}
}

// add registers one erased entry. It is reached through [Register], which is
// the only way to obtain an Entry.
func (r *Registry) add(e Entry) error {
	if ptr.IsNil(e) {
		return errdefs.Validationf("resource registry: nil factory")
	}
	spec := e.Spec()
	if err := spec.Validate(); err != nil {
		return err
	}
	key := spec.Key()
	if e.BuildType() == nil {
		return errdefs.Validationf(
			"resource registry: factory %s/%s has no build type", key.Kind, key.Impl)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[key]; dup {
		return errdefs.Conflictf(
			"resource registry: factory %s/%s already registered",
			key.Kind, key.Impl)
	}
	r.entries[key] = e
	return nil
}

// MustRegister registers f and panics on error. It is for tests and for
// packages that register a known-good factory set at init time; production
// wiring uses [Register] and returns the error.
func MustRegister[T any](r *Registry, f Factory[T]) {
	if err := Register(r, f); err != nil {
		panic(err)
	}
}

// Lookup returns the factory for kind/impl. An empty impl matches a
// factory registered with an empty impl.
func (r *Registry) Lookup(kind Kind, impl string) (Entry, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[Key{Kind: kind, Impl: impl}]
	return e, ok
}

// BuildType returns the Go type the factory for kind/impl produces.
func (r *Registry) BuildType(kind Kind, impl string) (reflect.Type, bool) {
	e, ok := r.Lookup(kind, impl)
	if !ok {
		return nil, false
	}
	return e.BuildType(), true
}

// BuildTypes returns every registered (Kind, Impl) with the Go type it
// produces.
func (r *Registry) BuildTypes() map[Key]reflect.Type {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[Key]reflect.Type, len(r.entries))
	for key, e := range r.entries {
		out[key] = e.BuildType()
	}
	return out
}

// Specs returns the registered factory specs in unspecified order.
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make([]Spec, 0, len(r.entries))
	for _, e := range r.entries {
		specs = append(specs, e.Spec())
	}
	return specs
}
