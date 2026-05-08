// Package sqlite implements engine.CheckpointStore on top of SQLite.
//
// The driver is modernc.org/sqlite (pure Go, no cgo) so this backend
// works on every Go platform without a C toolchain. The store is
// suitable for single-process daemons (vesseld), embedded examples,
// and tests; for multi-writer workloads use the postgres backend.
//
// # Usage
//
//	store, err := sqlite.Open(ctx, "file:checkpoints.db?_pragma=journal_mode(WAL)")
//	if err != nil { return err }
//	defer store.Close()
//
//	// Wire into engine.LoadAndResume / engine.Host.Checkpointer.
//
// # Schema
//
// The store keeps the latest checkpoint per ExecID. Older checkpoints
// for the same exec id are overwritten in place (UPSERT). Callers
// that need a full history should write their own append-only table
// — checkpoints are the engine's "where to resume from" record, not
// an audit log.
//
// Schema is created idempotently by [Open]; it is also exposed as
// [Migrate] for callers that want to run the migration explicitly
// (e.g. before a rolling deploy).
package sqlite
