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
// Migration notes (v0.2.0):
//
//   - This package was promoted from sdk/memory/ltm. Update imports
//     and qualifiers (ltm.X → recall.X). No symbol-level renames in
//     the lift itself; subsequent renames are noted on each type.
//   - Scope.SessionID was removed. It conflated conversation-thread
//     state (now in sdk/history) with long-term-fact partitioning;
//     callers should keep transient thread state in sdk/history or
//     model durable signals as Entry.Keywords. This is a breaking
//     schema change: deterministicEntryID no longer mixes the
//     session line, so re-ingest may produce duplicates of facts
//     that were extracted before the upgrade — treat the LTM index
//     as a fresh corpus.
//   - The MemoryAware / ContextAssembler glue was deleted. Compose
//     history.Memory + recall.Memory at the call site instead; see
//     examples/chatbot-with-recall for the canonical ~10-line
//     pattern (Recall → format hits → prepend system prompt).
package recall
