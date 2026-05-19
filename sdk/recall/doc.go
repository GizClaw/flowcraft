// Package recall is the v2 fact-centric long-term memory API.
//
// This package intentionally does not preserve the v1 Entry/retrieval-index
// implementation. The legacy implementation lives at sdk/recall_v1 and is
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
// The first v2 commit keeps the public surface deliberately small while the
// internal packages establish the final ownership boundaries.
package recall
