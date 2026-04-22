// Package sqlite provides a crash-recoverable JobQueue for sdk/memory/ltm
// .
//
// Schema:
//
//	CREATE TABLE memory_jobs (
//	    id           TEXT PRIMARY KEY,
//	    namespace    TEXT NOT NULL,
//	    payload      BLOB NOT NULL,         -- JSON
//	    state        TEXT NOT NULL,         -- pending/running/succeeded/failed/dead
//	    attempts     INT  NOT NULL DEFAULT 0,
//	    last_error   TEXT,
//	    entry_ids    TEXT,                  -- JSON array
//	    created_at   INTEGER NOT NULL,      -- unix-ms
//	    updated_at   INTEGER NOT NULL,
//	    next_run_at  INTEGER NOT NULL
//	);
//	CREATE INDEX memory_jobs_pending ON memory_jobs(state, next_run_at);
//
// On Open, any rows in state='running' are reset to 'pending' so that a worker
// crash does not strand a job — at-least-once is upheld and Index-level
// idempotency (deterministic doc IDs) prevents duplicate entries.
//
// Backend: pure-Go modernc.org/sqlite (cgo-free).
package sqlite
