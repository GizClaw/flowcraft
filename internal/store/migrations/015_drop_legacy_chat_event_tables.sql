-- +goose Up

-- R5 §12.3 cleanup: drop the two legacy "old world" tables that are
-- replaced by the event log + projector views.
--
--   * messages          → ChatProjector materialises messages from
--                         chat.message.sent envelopes.
--   * execution_events  → /workflows/runs/{id}/events deleted; consumers
--                         subscribe to GET /api/events?partition=runtime:<id>
--                         instead. The diagnostics fallback that scanned
--                         this table for error codes was also removed
--                         (workflow_runs.outputs is the canonical source).

DROP INDEX IF EXISTS idx_messages_conversation_id;
DROP INDEX IF EXISTS idx_messages_created_at;
DROP TABLE IF EXISTS messages;

DROP INDEX IF EXISTS idx_execution_events_run_id;
DROP TABLE IF EXISTS execution_events;

-- +goose Down

-- Recreate the tables in their 001_init shape so a goose-down still works
-- on a database that was migrated through 015. Data is not restored — the
-- legacy tables were drop-only after R5.

CREATE TABLE IF NOT EXISTS messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    role            TEXT NOT NULL,
    parts           TEXT NOT NULL DEFAULT '[]',
    token_count     INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation_id ON messages(conversation_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);

CREATE TABLE IF NOT EXISTS execution_events (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL,
    node_id    TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_execution_events_run_id ON execution_events(run_id);
