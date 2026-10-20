package deploy

import (
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Resolve returns the built value registered under name, narrowed to T.
//
// It is the typed read internal wiring should use: the caller names the type it
// expects, so a mismatch is reported once, here, instead of being re-asserted
// at every call site. [Result.Value] stays for host-level discovery, where the
// resource name comes from configuration and no type is known at compile time.
// Resolve is a free function because Go has no generic methods.
func Resolve[T any](result *Result, name string) (T, error) {
	var zero T
	if result == nil {
		return zero, errdefs.Validationf("deploy: Resolve on a nil result")
	}
	raw, ok := result.values[name]
	if !ok || raw == nil {
		return zero, errdefs.NotFoundf("deploy: resource %q is not built", name)
	}
	if ptr.IsNil(raw) {
		// A typed nil was built: the resource exists, it just produced nothing.
		return zero, errdefs.Validationf(
			"deploy: resource %q is a typed nil (%T), want %s",
			name, raw, reflect.TypeFor[T]())
	}
	value, ok := raw.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"deploy: resource %q is %T, want %s", name, raw, reflect.TypeFor[T]())
	}
	return value, nil
}

// Named is one built value together with the resource name it was registered
// under.
type Named[T any] struct {
	Name  string
	Value T
}

// Collect returns every built value that is a T, in construction order. It is
// the typed form of "walk the deployment and keep what implements X", used by
// post-build attachment passes that do not care which resource it was.
func Collect[T any](result *Result) []T {
	named := CollectNamed[T](result)
	out := make([]T, 0, len(named))
	for _, entry := range named {
		out = append(out, entry.Value)
	}
	return out
}

// CollectNamed is [Collect] with the resource names kept, for passes whose
// diagnostics have to say which resource caused a problem (a duplicate binder,
// for example).
func CollectNamed[T any](result *Result) []Named[T] {
	if result == nil {
		return nil
	}
	var out []Named[T]
	for _, name := range result.order {
		if value, ok := result.values[name].(T); ok && !ptr.IsNil(value) {
			out = append(out, Named[T]{Name: name, Value: value})
		}
	}
	return out
}
