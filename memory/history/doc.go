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
// a manual compaction. The in-tree tool wrappers (taking a Coordinator
// for per-conversation serialization) live in an adapter package
// outside sdk; this package exposes only the underlying primitives.
//
// # v0.3.0 surface
//
// The v0.3 surface narrows on the [History] / [Coordinator] interfaces.
// The following v0.2 entry points were removed in v0.3.0:
//
//   - Closer / compactor.Close — replaced by [Coordinator.Shutdown]
//     (context-aware, refuses late writes).
//   - Top-level RecoverArchive, SaveManifest helpers — folded into the
//     internal recoverArchiveImpl/saveManifestImpl helpers exercised by
//     [Coordinator]. The exported [Archive], [LoadManifest] and
//     [LoadArchivedMessages] survive for adapter packages that need
//     direct archive access; [Coordinator] auto-recovers in-flight
//     archives at construction.
//   - SummaryCacheStore — superseded by [SummaryStore] which the DAG
//     already consumes.
//   - In-package ToolDeps / RegisterTools — moved out of sdk into the
//     adapter layer.
//
// # Naming history
//
// This package was renamed from sdk/memory in v0.2.0. The previous
// Save(fullHistory) method was replaced by Append(newOnly) — the old
// signature was lossy under concurrent writers (read-modify-write race)
// and silently accepted truncated histories. There is no compat shim;
// call sites pass only the freshly produced messages.
package history
