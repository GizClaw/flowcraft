// Package ltm is the long-term-memory implementation built on top of
// the unified retrieval layer.
//
// Compared with the legacy sdk/memory.LongTermStore + extractor pipeline:
//
//   - Single-pass Additive extraction (no merge/delete decisions);
//   - Entity linking via Doc.Metadata["entities"] consumed by EntityBoost;
//   - Async Save with a persistent JobQueue (default: in-memory; SQLite is
//     provided in sdkx/memory/jobqueue/sqlite);
//   - AgentID dimension on MemoryScope (soft isolation via metadata);
//   - History / Rollback / Forget backed by retrieval/journal;
//   - TTL via MemoryEntry.ExpiresAt + Sweeper for physical cleanup.
//
// Construct via ltm.New(cfg) and use the Memory interface — the legacy
// sdk/memory package keeps working unchanged for callers that still want it.
package ltm
