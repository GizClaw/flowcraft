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
//   - [Store] is the persistence interface. The package ships
//     [InMemoryStore] and [NewFileStore]; bring your own for Redis,
//     Postgres, etc.
//   - [SummaryDAG] / [FileSummaryStore] are the building blocks behind
//     compacted's hierarchical summarization. Most callers do not touch
//     them directly.
//
// # Tools
//
// Two graph tools surface this package to LLMs: history_expand fetches
// the verbatim messages behind a summary node, history_compact triggers
// a manual compaction. They are wired through [NewExpandTool] /
// [NewCompactTool].
//
// # Migration note
//
// This package was renamed from sdk/memory in v0.2.0. The previous
// Save(fullHistory) method was replaced by Append(newOnly) — the old
// signature was lossy under concurrent writers (read-modify-write race)
// and silently accepted truncated histories. There is no compat shim;
// call sites pass only the freshly produced messages.
package history
