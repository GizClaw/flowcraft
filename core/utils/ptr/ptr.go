// Package ptr holds the two reflection helpers the module needs everywhere and
// no single package owns: a defensive pointer copy and a typed-nil check.
//
// It imports nothing beyond the standard library — not even another core
// package — so base packages such as core/message can use it without
// inheriting the dependency graph of core/utils (HTTP, TLS, telemetry,
// configuration decoding).
package ptr

import "reflect"

// Clone returns a pointer to a copy of the value, or nil for a nil pointer. It
// is the shared defensive-copy helper for canonical value types: every Clone
// method that owns an optional field clones the pointed-to value rather than
// sharing it, so a caller mutating its own copy cannot reach into the
// runtime's.
func Clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

// IsNil reports whether value is nil, including a typed nil sitting behind an
// interface — for example a (*T)(nil) boxed in an any, which compares unequal
// to nil. reflect.Value.IsNil only accepts chan, func, interface, map, pointer
// and slice kinds; every other kind is not nilable, so it answers false.
func IsNil(value any) bool {
	if value == nil {
		return true
	}
	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	}
	return false
}
