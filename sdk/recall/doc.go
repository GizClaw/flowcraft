// Package recall implements long-term agent memory on top of the
// unified retrieval layer. It owns extraction (turning conversation
// turns into structured facts), persistence (Entry rows in a retrieval
// Index, scoped per runtime/agent/user), and retrieval (BM25 + vector
// hybrid search with entity boost and TTL filtering).
//
// Capabilities:
//
//   - Single-pass additive extraction; supersedence decisions happen at
//     write time via soft-merge.
//   - Entity linking via Doc.Metadata["entities"], consumed by
//     EntityBoost.
//   - Sync Save and async SaveAsync backed by a [JobQueue] (default
//     in-memory; sdkx/recall/jobqueue/sqlite provides a durable
//     SQLite queue).
//   - Three-axis [Scope] (RuntimeID + AgentID + UserID) plus a
//     [Partitions] selector that controls whether a recall visits the
//     per-user bucket, the runtime-global bucket, or both.
//   - History / Rollback / Forget backed by retrieval/journal.
//   - TTL via Entry.ExpiresAt plus an optional sweeper goroutine.
//
// Construct via [New] with [Option] modifiers. Callers wanting context
// injection into a chat prompt should compose this package with
// sdk/history themselves; see examples/chatbot-with-recall for a
// reference assembler.
//
// Migration history: this package was promoted from sdk/memory/ltm in
// v0.2.0. The same release dropped Scope.SessionID and the
// MemoryAware/ContextAssembler glue; see docs/memory-refactor.md for
// the full migration guide.
package recall
