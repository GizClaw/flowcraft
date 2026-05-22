// Package recall is the v2 fact-centric long-term memory API.
//
// This package intentionally does not preserve the v1 Entry/retrieval-index
// implementation. The legacy implementation lives at sdk/recall and is
// deprecated for v0.5.0 removal.
//
// Architectural direction:
//
//   - Temporal facts are the canonical write unit.
//   - Stores own durable truth; projections are rebuildable derived views.
//   - Retrieval backends are optional projections/sources, not the root of
//     memory state.
//   - Recall reads through planner -> candidate sources -> fusion ->
//     materialization -> final context.
//
// Lifecycle and ownership:
//
//   - New does not start background goroutines. Side-effect and async semantic
//     work is caller-driven via NewSideEffectProcessor and
//     NewAsyncSemanticProcessor; worker loops live in the host or memory ops
//     packages.
//   - Memory.Close releases the canonical TemporalStore, optional
//     EvidenceStore, and retrieval.Index wired into the Memory, including
//     injected implementations. Callers that share those backends across Memory
//     instances should coordinate ownership outside this package.
//   - SideEffectOutbox and AsyncSemanticQueue intentionally have no Close or
//     Drain method in the core contract. Their shutdown/drain semantics belong
//     to the caller-owned worker loop or durable adapter.
//   - Reconciler is an explicit operator API, not a scheduler. It rebuilds or
//     audits derived work from canonical facts only when callers invoke it.
package recall
