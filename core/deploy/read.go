package deploy

import (
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Resolve returns the built value registered under name, narrowed to T.
//
// It replaces a stringly "Value(name) (any, bool)" accessor: the caller names
// the type it expects, so a mismatch is reported once, here, instead of being
// re-asserted at every call site. It is a free function because Go has no
// generic methods.
func Resolve[T any](result *Result, name string) (T, error) {
	var zero T
	if result == nil {
		return zero, errdefs.Validationf("deploy: Resolve on a nil result")
	}
	raw, ok := result.values[name]
	if !ok || raw == nil || ptr.IsNil(raw) {
		return zero, errdefs.NotFoundf("deploy: resource %q is not built", name)
	}
	value, ok := raw.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"deploy: resource %q is %T, want %s", name, raw, reflect.TypeFor[T]())
	}
	return value, nil
}

// Collect returns every built value that is a T, in construction order. It is
// the typed form of "walk the deployment and keep what implements X", used by
// post-build attachment passes.
func Collect[T any](result *Result) []T {
	if result == nil {
		return nil
	}
	var out []T
	for _, name := range result.order {
		if value, ok := result.values[name].(T); ok && !ptr.IsNil(value) {
			out = append(out, value)
		}
	}
	return out
}
