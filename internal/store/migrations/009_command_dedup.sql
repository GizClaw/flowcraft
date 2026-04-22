-- +goose Up

-- command_dedup: generic idempotency table for command handlers.
CREATE TABLE IF NOT EXISTS command_dedup (
    command_id   TEXT PRIMARY KEY,
    executed_at  TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE IF EXISTS command_dedup;
