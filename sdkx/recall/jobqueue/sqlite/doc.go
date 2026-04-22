// Package sqlite provides a crash-recoverable [recall.JobQueue]
// implementation backed by SQLite.
//
// Schema:
//
//	CREATE TABLE recall_jobs (
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
//	CREATE INDEX recall_jobs_pending ON recall_jobs(state, next_run_at);
//
// On Open, any rows in state='running' are reset to 'pending' so that a worker
// crash does not strand a job — at-least-once is upheld and Index-level
// idempotency (deterministic doc IDs) prevents duplicate entries.
//
// Backend: pure-Go modernc.org/sqlite (cgo-free).
//
// Migration history: this package was renamed from
// sdkx/memory/jobqueue/sqlite in v0.2.0 alongside the larger
// sdk/memory → sdk/recall split. The on-disk table was renamed
// memory_jobs → recall_jobs to match. Existing deployments must drain
// their old queue before upgrading or run a one-shot
// `ALTER TABLE memory_jobs RENAME TO recall_jobs;
//  ALTER INDEX memory_jobs_pending RENAME TO recall_jobs_pending;`
// against the SQLite file.
package sqlite
