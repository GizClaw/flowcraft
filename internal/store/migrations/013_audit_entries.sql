-- +goose Up

-- audit_entries: audit log view entries derived from event_log.
-- Only stores the audit-required subset; full envelope lives in event_log (same seq).
CREATE TABLE IF NOT EXISTS audit_entries (
    seq         INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    actor_id    TEXT,
    actor_kind  TEXT,
    actor_realm_id TEXT,
    actor_json  TEXT,
    ts          TEXT NOT NULL,
    partition   TEXT,
    trace_id    TEXT,
    summary     TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_audit_actor_ts ON audit_entries(actor_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_type_ts  ON audit_entries(type, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_trace   ON audit_entries(trace_id);
CREATE INDEX IF NOT EXISTS idx_audit_partition_ts ON audit_entries(partition, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_realm_ts ON audit_entries(actor_realm_id, ts DESC);

-- +goose Down
DROP TABLE IF EXISTS audit_entries;
