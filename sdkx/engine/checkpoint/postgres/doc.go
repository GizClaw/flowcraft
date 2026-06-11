// Package postgres implements engine.CheckpointStore on top of
// PostgreSQL.
//
// The driver is github.com/jackc/pgx/v5 (used directly via pgxpool;
// no database/sql layer). Suitable for multi-process services,
// horizontally scaled deployments, and production durability
// requirements where SQLite's single-writer model is insufficient.
//
// # Usage
//
//	store, err := postgres.Open(ctx, "postgres://flowcraft:secret@db:5432/flowcraft?sslmode=disable")
//	if err != nil { return err }
//	defer store.Close()
//
//	// Wire into engine.LoadAndResume / engine.Host.Checkpointer.
//
// # Schema
//
// One row per ExecID; Save uses INSERT ... ON CONFLICT DO UPDATE so
// the table always holds the latest checkpoint per execution.
// Migrations run idempotently inside [Open]; expose [Migrate] for
// callers that want explicit control.
package postgres
