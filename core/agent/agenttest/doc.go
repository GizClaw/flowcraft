// Package agenttest provides reusable contract-test machinery for
// the interfaces declared in core/agent — [agent.Engine], [agent.Host],
// [agent.Observer], [agent.Referee], [agent.CheckpointStore],
// [agent.Resumer], [agent.StreamSink], [agent.Preparer],
// [agent.Committer], [agent.CommitViewProvider],
// [agent.CheckpointSuggester] and [agent.ScriptRuntime].
//
// Modeled on net/http/httptest, testing/iotest, and
// gocloud.dev/blob/drivertest: a single sibling package next to its
// subject covers every contract-checking surface the parent package
// exposes. Importable from any concrete implementation's *_test
// files; contains exclusively test-support code; nothing in here
// should be referenced from non-test production paths.
//
// # What lives here
//
//   - [EngineSuite] — the standard contract every [agent.Engine]
//     implementation should pass. New engines add a one-liner:
//
//     func TestEngineContract(t *testing.T) {
//     agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
//     return newMyEngine(), agenttest.Capabilities{}
//     })
//     }
//
//   - [HostSuite] — the standard contract every [agent.Host]
//     implementation should pass:
//
//     func TestMyHost_Contract(t *testing.T) {
//     agenttest.HostSuite(t, func() agent.Host { return newMyHost() })
//     }
//
//   - [ObserverSuite] — the standard contract every [agent.Observer]
//     implementation should pass (no input mutation, prompt return,
//     concurrent safety).
//
//   - [RefereeSuite] — the standard contract every
//     [agent.Referee] implementation should pass.
//
//   - [CheckpointStoreSuite] — the shared conformance suite every
//     [agent.CheckpointStore] implementation should pass, including
//     the optional [agent.CheckpointLister] / [agent.CheckpointDeleter]
//     extensions.
//
//   - [ResumerSuite] — the contract every [agent.Resumer]
//     implementation should pass (cheap, classified, mutation-free,
//     concurrent-safe).
//
//   - [StreamSinkSuite] — the contract every [agent.StreamSink]
//     implementation should pass (panic-free, ctx-observing,
//     concurrent-safe).
//
//   - [PreparerSuite] — the contract every [agent.Preparer]
//     implementation should pass (non-nil fresh boards, no input
//     mutation, ctx-observing).
//
//   - [CommitterSuite] — the contract every [agent.Committer]
//     implementation should pass (no input mutation, ctx-observing).
//
//   - [CommitViewProviderSuite] — the contract every
//     [agent.CommitViewProvider] implementation should pass (non-nil
//     projected board, no input mutation, ctx-observing).
//
//   - [CheckpointSuggesterSuite] — the contract every
//     [agent.CheckpointSuggester] implementation should pass
//     (panic-free, prompt return).
//
//   - [ScriptRuntimeSuite] — the contract every [agent.ScriptRuntime]
//     implementation should pass (panic-free, executes a caller-supplied
//     minimal fixture, concurrent-safe).
//
//   - [MockHost] — a minimal Host implementation that records every
//     interaction, lets tests inject interrupts / user replies /
//     publish-checkpoint-usage errors, and exposes the captured
//     envelopes / usage / checkpoints for assertion. Engines may use
//     it directly in their own tests instead of re-implementing the
//     full Host surface.
//
// # What does NOT live here
//
// This package does not enumerate engine- or host-specific
// behaviours (graph-edge ordering, script-language semantics,
// application-specific sandbox bus routing, …). It only asserts what
// every implementation of the targeted interface is contractually
// obliged to do. Implementation-specific tests live next to the
// implementation.
package agenttest
