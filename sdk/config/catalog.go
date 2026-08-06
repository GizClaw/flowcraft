package config

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Factory builds one output value of a named extension from its input.
// Extensions own the decoding of their settings and the meaning of
// their output; the catalog owns lookup and validation only.
type Factory[In, Out any] func(ctx context.Context, in In) (Out, error)

// Catalog is an instance-local registry of named factories. It is the
// generic shape behind every module's extension point: workspace
// drivers, tool middleware kinds, sandbox backends, and inference
// providers all register a name and build by name. There is no global
// registration: two Catalogs in one process stay independent.
type Catalog[In, Out any] struct {
	factories map[string]Factory[In, Out]
}

// NewCatalog returns an empty catalog.
func NewCatalog[In, Out any]() *Catalog[In, Out] {
	return &Catalog[In, Out]{factories: make(map[string]Factory[In, Out])}
}

// Register adds a named factory. Empty names, nil factories, and
// duplicates are validation errors.
func (c *Catalog[In, Out]) Register(name string, factory Factory[In, Out]) error {
	if c == nil {
		return errdefs.Validationf("config catalog: catalog is nil")
	}
	if name == "" {
		return errdefs.Validationf("config catalog: factory name is empty")
	}
	if factory == nil {
		return errdefs.Validationf("config catalog: factory %q is nil", name)
	}
	if _, exists := c.factories[name]; exists {
		return errdefs.Validationf(
			"config catalog: factory %q is already registered", name)
	}
	c.factories[name] = factory
	return nil
}

// Build invokes the named factory. A missing name is a NotFound error;
// a factory returning a nil output (for nilable output kinds) is an
// Internal error. Factory errors are wrapped so their classification
// survives.
func (c *Catalog[In, Out]) Build(ctx context.Context, name string, in In) (Out, error) {
	var zero Out
	if c == nil {
		return zero, errdefs.Validationf("config catalog: catalog is nil")
	}
	if ctx == nil {
		return zero, errdefs.Validationf("config catalog: context is nil")
	}
	factory, ok := c.factories[name]
	if !ok {
		return zero, errdefs.NotFoundf(
			"config catalog: factory %q is not registered", name)
	}
	out, err := factory(ctx, in)
	if err != nil {
		return zero, fmt.Errorf("config catalog factory %q: %w", name, err)
	}
	if isNil(out) {
		return zero, errdefs.Internalf(
			"config catalog: factory %q returned nil", name)
	}
	return out, nil
}

// Names returns registered factory names in stable sorted order.
func (c *Catalog[In, Out]) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.factories))
	for name := range c.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
