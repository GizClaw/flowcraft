-- +goose Up

-- event_log: the append-only event store.
-- seq uses INTEGER PRIMARY KEY AUTOINCREMENT so gaps from ROLLBACK/retention
-- are expected and tolerated by consumers.
CREATE TABLE IF NOT EXISTS event_log (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    partition      TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    version        INTEGER NOT NULL,
    category       TEXT    NOT NULL,
    ts             TEXT    NOT NULL,
    payload        BLOB    NOT NULL,
    trace_id       TEXT,
    span_id        TEXT,
    actor_id       TEXT,
    actor_kind     TEXT,
    actor_realm_id TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_event_log_partition_seq ON event_log(partition, seq);
CREATE INDEX IF NOT EXISTS idx_event_log_ts            ON event_log(ts);
CREATE INDEX IF NOT EXISTS idx_event_log_category_ts   ON event_log(category, ts);
CREATE INDEX IF NOT EXISTS idx_event_log_actor         ON event_log(actor_kind, actor_id);

-- +goose Down
DROP TABLE IF EXISTS event_log;
