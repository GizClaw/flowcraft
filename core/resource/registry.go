package resource

import "github.com/GizClaw/flowcraft/core/errdefs"

// Key is the unique (Kind, Impl) identity of a factory in a registry.
type Key struct {
	Kind Kind
	Impl string
}

// Registry maps resource kinds (+ implementations) to factories. Each
// resource module registers its factories explicitly; there is no
// global state.
type Registry struct {
	factories map[Key]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[Key]Factory)}
}

// Register adds a factory, validating its spec and rejecting a
// duplicate (Kind, Impl) key.
func (r *Registry) Register(f Factory) error {
	if f == nil {
		return errdefs.Validationf("resource registry: nil factory")
	}
	spec := f.Spec()
	if err := spec.Validate(); err != nil {
		return err
	}
	key := Key{Kind: spec.Kind, Impl: spec.Impl}
	if _, dup := r.factories[key]; dup {
		return errdefs.Conflictf(
			"resource registry: factory %s/%s already registered",
			spec.Kind, spec.Impl)
	}
	r.factories[key] = f
	return nil
}

// MustRegister registers f and panics on error.
func (r *Registry) MustRegister(f Factory) {
	if err := r.Register(f); err != nil {
		panic(err)
	}
}

// Lookup returns the factory for kind/impl. An empty impl matches a
// factory registered with an empty impl.
func (r *Registry) Lookup(kind Kind, impl string) (Factory, bool) {
	f, ok := r.factories[Key{Kind: kind, Impl: impl}]
	return f, ok
}

// Specs returns the registered factory specs in unspecified order.
func (r *Registry) Specs() []Spec {
	specs := make([]Spec, 0, len(r.factories))
	for _, f := range r.factories {
		specs = append(specs, f.Spec())
	}
	return specs
}
