// Package enginetest provides reusable contract-test machinery for
// implementations of [engine.Engine].
//
// Modeled on net/http/httptest and testing/iotest: a sibling package
// next to its subject, importable from any concrete engine's *_test
// files. It contains exclusively test-support code; nothing in here
// should be referenced from non-test production paths.
//
// # What lives here
//
//   - [Suite] / [RunSuite] — the standard contract every Engine
//     implementation should pass. New engines should add a one-liner:
//
//     func TestEngineContract(t *testing.T) {
//     enginetest.RunSuite(t, func() engine.Engine { return newMyEngine() })
//     }
//
//   - [MockHost] — a minimal Host implementation that records every
//     interaction, lets tests inject interrupts / user replies, and
//     exposes the captured envelopes / usage / checkpoints for
//     assertion. Engines may use it directly in their own tests
//     instead of re-implementing the full Host surface.
//
// # What does NOT live here
//
// This package does not enumerate engine-specific behaviours
// (graph-edge ordering, script-language semantics, …). It only
// asserts what every [engine.Engine] is contractually obliged to do.
// Engine-specific tests live next to the engine.
package enginetest
