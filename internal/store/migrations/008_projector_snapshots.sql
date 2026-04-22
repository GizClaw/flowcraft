-- +goose Up

-- projector_snapshots: named snapshots for fast projector restore.
CREATE TABLE IF NOT EXISTS projector_snapshots (
    projector_name TEXT NOT NULL,
    cursor_seq     INTEGER NOT NULL,
    payload_fmt    INTEGER NOT NULL,
    payload        BLOB    NOT NULL,
    created_at     TEXT    NOT NULL,
    PRIMARY KEY (projector_name, cursor_seq)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_projector_snapshots_name_seq
    ON projector_snapshots(projector_name, cursor_seq DESC);

-- +goose Down
DROP TABLE IF EXISTS projector_snapshots;
