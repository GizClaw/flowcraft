// Package recall implements long-term agent memory on top of the unified
// retrieval layer. It owns extraction (LLM facts out of conversation
// turns), persistence (versioned MemoryEntry rows in a retrieval Index),
// and retrieval (BM25 + vector hybrid search with entity boost).
//
// Capabilities:
//
//   - Single-pass additive extraction (no merge/delete decisions in the
//     extractor; supersedence is decided at write time).
//   - Entity linking via Doc.Metadata["entities"], consumed by EntityBoost.
//   - Async Save with a persistent JobQueue (default: in-memory; a SQLite
//     queue is provided in sdkx/recall/jobqueue/sqlite — moved in a
//     follow-up commit).
//   - AgentID dimension on MemoryScope for soft isolation via metadata.
//   - History / Rollback / Forget backed by retrieval/journal.
//   - TTL via MemoryEntry.ExpiresAt plus a Sweeper for physical cleanup.
//
// Construct via recall.New(cfg) and consume through the Memory interface.
//
// Migration: this package was promoted from sdk/memory/ltm in v0.2.0.
// Identifiers are unchanged in this rename commit; later commits in the
// same series rework the constructor (Config -> functional options) and
// drop the MemoryAware/Assembler glue layer in favor of a thin example.
package recall
