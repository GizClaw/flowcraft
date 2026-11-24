// Package ptr provides small generic pointer helpers for the legacy driver
// implementations. It exists because core/internal is not importable from the
// driver module.
package ptr

import "reflect"

// Clone returns a copy of value, or nil when value is nil.
func Clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
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
