// Package conformance ships a table-driven contract suite that any
// engine.CheckpointStore implementation can run against itself to
// prove it satisfies the [engine.CheckpointStore] (and the optional
// [engine.CheckpointLister] / [engine.CheckpointDeleter]) contracts.
//
// Backends embed the suite from their own *_test.go:
//
//	func TestStore(t *testing.T) {
//	    conformance.RunSuite(t, func(t *testing.T) engine.CheckpointStore {
//	        return openTestStore(t)
//	    })
//	}
//
// The suite intentionally lives under sdkx so first-party backends
// (sqlite, postgres) can re-use it without requiring the sdk module
// to ship test code. Users writing their own backend can also import
// it (sdkx is already in their require graph if they consume any
// sdkx package).
package conformance
