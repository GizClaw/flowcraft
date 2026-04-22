-- +goose Up

-- projector_checkpoints: tracks each projector consumer's read cursor.
-- Set only via UnitOfWork.CheckpointSet (never exposed publicly).
CREATE TABLE IF NOT EXISTS projector_checkpoints (
    projector_name TEXT PRIMARY KEY,
    checkpoint_seq INTEGER NOT NULL,
    updated_at     TEXT    NOT NULL
) STRICT;

-- +goose Down
DROP TABLE IF EXISTS projector_checkpoints;
