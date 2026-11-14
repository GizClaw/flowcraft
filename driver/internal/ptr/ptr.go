// Package ptr provides small generic pointer helpers for the legacy driver
// implementations. It exists because core/internal is not importable from the
// driver module.
package ptr

// Clone returns a copy of value, or nil when value is nil.
func Clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
