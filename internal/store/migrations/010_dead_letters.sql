-- +goose Up

-- dead_letters: permanently failed events归档.
CREATE TABLE IF NOT EXISTS dead_letters (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    projector_name  TEXT    NOT NULL,
    event_seq       INTEGER NOT NULL,
    event_type      TEXT    NOT NULL,
    error_class     TEXT    NOT NULL,
    error_message   TEXT    NOT NULL,
    envelope        BLOB    NOT NULL,
    created_at      TEXT    NOT NULL,
    resolved_at     TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_dead_letters_unresolved
    ON dead_letters(projector_name, resolved_at) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS dead_letters;
