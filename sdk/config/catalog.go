package config

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Factory builds one output value of a named extension from its input.
// Extensions own the decoding of their settings and the meaning of
// their output; the registry owns lookup and validation only.
type Func[In, Out any] func(ctx context.Context, in In) (Out, error)

// Registry is an instance-local registry of named factories. It is
// the generic shape behind every module's extension point: workspace
// drivers, tool middleware kinds, sandbox backends, and inference
// providers all register a name and build by name. There is no global
// registration: two Registries in one process stay independent.
type Registry[In, Out any] struct {
	factories map[string]Func[In, Out]
}

// NewRegistry returns an empty registry.
func NewRegistry[In, Out any]() *Registry[In, Out] {
	return &Registry[In, Out]{factories: make(map[string]Func[In, Out])}
}

// Register adds a named factory. Empty names, nil factories, and
// duplicates are validation errors.
func (r *Registry[In, Out]) Register(name string, factory Func[In, Out]) error {
	if r == nil {
		return errdefs.Validationf("config registry: registry is nil")
	}
	if name == "" {
		return errdefs.Validationf("config registry: factory name is empty")
	}
	if factory == nil {
		return errdefs.Validationf("config registry: factory %q is nil", name)
	}
	if _, exists := r.factories[name]; exists {
		return errdefs.Validationf(
			"config registry: factory %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

// Build invokes the named factory. A missing name is a NotFound error;
// a factory returning a nil output (for nilable output kinds) is an
// Internal error. Factory errors are wrapped so their classification
// survives.
func (r *Registry[In, Out]) Build(ctx context.Context, name string, in In) (Out, error) {
	var zero Out
	if r == nil {
		return zero, errdefs.Validationf("config registry: registry is nil")
	}
	if ctx == nil {
		return zero, errdefs.Validationf("config registry: context is nil")
	}
	factory, ok := r.factories[name]
	if !ok {
		return zero, errdefs.NotFoundf(
			"config registry: factory %q is not registered", name)
	}
	out, err := factory(ctx, in)
	if err != nil {
		return zero, fmt.Errorf("config registry factory %q: %w", name, err)
	}
	if isNil(out) {
		return zero, errdefs.Internalf(
			"config registry: factory %q returned nil", name)
	}
	return out, nil
}

// Names returns registered factory names in stable sorted order.
func (r *Registry[In, Out]) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Catalog is the unified build-factory registry: [Factory] values
// registered under their Spec's (Kind, Impl) key. It is the single
// place an assembly engine keeps every build step — resources, engine
// kinds, and lifecycle hooks — so one lookup serves the whole
// document. There is no global registration: two Catalogs in one
// process stay independent.
type Catalog struct {
	mu        sync.RWMutex
	factories map[catalogKey]Factory
	specs     map[catalogKey]Spec
}

type catalogKey struct {
	kind string
	impl string
}

// NewCatalog returns an empty build catalog.
func NewCatalog() *Catalog {
	return &Catalog{
		factories: make(map[catalogKey]Factory),
		specs:     make(map[catalogKey]Spec),
	}
}

// Register adds f under its Spec's (Kind, Impl). A nil factory, an
// invalid spec, or a duplicate key is a validation error.
func (c *Catalog) Register(f Factory) error {
	if c == nil {
		return errdefs.Validationf("config catalog: catalog is nil")
	}
	if f == nil {
		return errdefs.Validationf("config catalog: nil factory")
	}
	spec := f.Spec()
	if err := spec.Validate(); err != nil {
		return err
	}
	key := catalogKey{kind: spec.Kind, impl: spec.Impl}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.factories[key]; dup {
		return errdefs.Validationf(
			"config catalog: %s/%s already registered",
			spec.Kind, spec.Impl)
	}
	c.factories[key] = f
	c.specs[key] = spec.Clone()
	return nil
}

// MustRegister is Register that panics on error — for init-time
// registration where a failure is a programming bug.
func (c *Catalog) MustRegister(f Factory) {
	if err := c.Register(f); err != nil {
		panic(err)
	}
}

// Lookup returns the factory registered under (kind, impl).
func (c *Catalog) Lookup(kind, impl string) (Factory, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	f, ok := c.factories[catalogKey{kind: kind, impl: impl}]
	return f, ok
}

// Specs returns defensive copies of every registered factory spec in
// stable kind-then-impl order.
func (c *Catalog) Specs() []Spec {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Spec, 0, len(c.specs))
	for _, spec := range c.specs {
		out = append(out, spec.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].Impl < out[j].Impl
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Keys returns every registered (kind, impl) key in stable order.
func (c *Catalog) Keys() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.factories))
	for key := range c.factories {
		keys = append(keys, key.kind+"/"+key.impl)
	}
	sort.Strings(keys)
	return keys
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
