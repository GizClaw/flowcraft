// Package history manages conversation transcripts: append messages,
// load them back fitted to a model's context window, and (optionally)
// compact older turns into hierarchical summaries to keep that window
// finite. Long-term fact recall lives in [sdk/recall].
//
// # Layering
//
//   - [History] is the strategy interface returned by [NewBuffer] and
//     [NewCompacted]. Pick Buffer for short sessions or tests; pick
//     Compacted when a conversation needs to outgrow a single context
//     window.
//   - [Coordinator] is the lifecycle + maintenance interface that the
//     compacted [History] additionally satisfies. It exposes Compact /
//     Archive / Shutdown and serializes them per conversation against
//     [History.Append] and the background ingest/archive worker.
//   - [Store] is the persistence interface. The package ships
//     [InMemoryStore] and [NewFileStore]; bring your own for Redis,
//     Postgres, etc.
//   - [SummaryDAG] / [FileSummaryStore] are the building blocks behind
//     compacted's hierarchical summarization. Most callers do not touch
//     them directly.
//
// # Lifecycle
//
// [NewBuffer] returns a stateless History; nothing to drain. The
// History returned by [NewCompacted] owns one serial worker goroutine
// per conversation plus a startup archive-recovery goroutine. Callers
// that own its lifetime should type-assert to [Coordinator] and call
// Shutdown(ctx) on shutdown so the queues drain cleanly:
//
//	hist := history.NewCompacted(store, llm, ws)
//	if coord, ok := hist.(history.Coordinator); ok {
//	    defer func() {
//	        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	        defer cancel()
//	        _ = coord.Shutdown(ctx)
//	    }()
//	}
//
// # Tools
//
// Two graph tools surface this package to LLMs: history_expand fetches
// the verbatim messages behind a summary node, history_compact triggers
// a manual compaction. Wire them through [RegisterTools] with a
// [ToolDeps] whose Coordinator field is populated, so the tools observe
// per-conversation serialization rather than mutating raw stores.
//
// # Migration to v0.3.0
//
// The v0.3 surface narrows on the [History] / [Coordinator] interfaces.
// The following v0.2 entry points are now Deprecated and will be removed
// in v0.3.0:
//
//   - [Closer] / [compactor].Close — replaced by
//     [Coordinator.Shutdown] (context-aware, refuses late writes).
//   - Top-level [Archive], [RecoverArchive], [LoadArchivedMessages],
//     [LoadManifest], [SaveManifest] — replaced by [Coordinator] (which
//     also auto-recovers in-flight archives at construction).
//   - [SummaryCacheStore] — superseded by [SummaryStore] which the DAG
//     already consumes; nothing in the package reads SummaryCacheStore.
//
// [ToolDeps] / [RegisterTools] stay supported in v0.3 — they now take
// an optional Coordinator field that, when set, makes history_compact
// route through the per-conversation queue.
//
// All deprecated symbols live in deprecated.go and continue to compile
// against the v0.2 ABI for one release.
//
// # Naming history
//
// This package was renamed from sdk/memory in v0.2.0. The previous
// Save(fullHistory) method was replaced by Append(newOnly) — the old
// signature was lossy under concurrent writers (read-modify-write race)
// and silently accepted truncated histories. There is no compat shim;
// call sites pass only the freshly produced messages.
package history
