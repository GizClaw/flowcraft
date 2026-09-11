package model

// ClonePointer returns a pointer to a copy of the value, or nil for a nil
// pointer. It exists so that every defensive copy in the module — this
// package's own Clone methods, the inference contract's value types, and the
// route policy's score structs — shares one implementation instead of
// carrying a private copy each. It lives here rather than in core/inference
// because this package is a leaf: both the contract and its decorators can
// import it, while importing the contract from here would be a cycle.
func ClonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
