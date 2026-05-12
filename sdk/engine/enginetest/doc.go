// Package enginetest provides reusable contract-test machinery for
// the interfaces declared in sdk/engine — [engine.Engine] and
// [engine.Host] today, more if the engine package grows.
//
// Modeled on net/http/httptest, testing/iotest, and
// gocloud.dev/blob/drivertest: a single sibling package next to its
// subject covers every contract-checking surface the parent
// package exposes. The Go convention is one xxxtest sub-package per
// parent package, not one per interface — see the stdlib examples
// linked above. Importable from any concrete implementation's
// *_test files; contains exclusively test-support code; nothing in
// here should be referenced from non-test production paths.
//
// # What lives here
//
//   - [RunSuite] — the standard contract every [engine.Engine]
//     implementation should pass. New engines add a one-liner:
//
//     func TestEngineContract(t *testing.T) {
//     enginetest.RunSuite(t, func() (engine.Engine, enginetest.Capabilities) {
//     return newMyEngine(), enginetest.Capabilities{}
//     })
//     }
//
//   - [HostSuite] — the standard contract every [engine.Host]
//     implementation should pass. Hosts (NoopHost, vessel
//     sandboxHost, OTel-instrumented HostFuncs, third-party hosts)
//     add a one-liner:
//
//     func TestMyHost_Contract(t *testing.T) {
//     enginetest.HostSuite(t, func() engine.Host { return newMyHost() })
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
// This package does not enumerate engine- or host-specific
// behaviours (graph-edge ordering, script-language semantics,
// vessel sandbox-specific bus routing, …). It only asserts what
// every implementation of the targeted interface is contractually
// obliged to do. Implementation-specific tests live next to the
// implementation.
package enginetest
