package component

import "fmt"

// StateError reports that a projection lane's persisted state exists but could
// not be materialized: the store holds a manifest for the scope, and reading
// the state that manifest points at failed a digest check, a decode, or the
// read itself.
//
// It is deliberately not what a lane with no state reports. That one is
// storage.ErrNotFound, and it means the scope was never indexed, so a request
// is answered from the lanes that do hold state without anything being wrong.
// This error means a lane that was supposed to answer could not, and its
// recall is missing from the result: a request must not report that as a lane
// that found nothing. Verify names the condition and Repair is the way out.
type StateError struct {
	// Projection names the state that cannot be read -- the storage name of
	// the lane, not the lane's catalog name.
	Projection string
	Err        error
}

func (state *StateError) Error() string {
	return fmt.Sprintf("memory component: %s: state cannot be read: %v", state.Projection, state.Err)
}

func (state *StateError) Unwrap() error { return state.Err }
