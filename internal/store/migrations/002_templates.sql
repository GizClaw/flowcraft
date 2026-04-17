-- +goose Up

CREATE TABLE IF NOT EXISTS templates (
    name        TEXT PRIMARY KEY,
    label       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL DEFAULT '',
    parameters  TEXT NOT NULL DEFAULT '[]',
    graph_def   TEXT NOT NULL,
    is_builtin  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS templates;
