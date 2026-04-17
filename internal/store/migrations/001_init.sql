-- +goose Up

-- Agents (formerly "apps")
CREATE TABLE IF NOT EXISTS agents (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL DEFAULT 'workflow',
    description   TEXT NOT NULL DEFAULT '',
    config        TEXT NOT NULL DEFAULT '{}',
    strategy      TEXT,
    input_schema  TEXT,
    output_schema TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Conversations
CREATE TABLE IF NOT EXISTS conversations (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    runtime_id TEXT NOT NULL DEFAULT '',
    variables  TEXT NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversations_agent_id ON conversations(agent_id);

-- Messages
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

-- Workflow runs
CREATE TABLE IF NOT EXISTS workflow_runs (
    id              TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    actor_id        TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    input           TEXT NOT NULL DEFAULT '',
    output          TEXT NOT NULL DEFAULT '',
    inputs          TEXT NOT NULL DEFAULT '{}',
    outputs         TEXT NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'running',
    usage           TEXT,
    elapsed_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_agent_id ON workflow_runs(agent_id);

-- Execution events
CREATE TABLE IF NOT EXISTS execution_events (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL,
    node_id    TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_execution_events_run_id ON execution_events(run_id);

-- Datasets
CREATE TABLE IF NOT EXISTS datasets (
    id             TEXT PRIMARY KEY,
    agent_id       TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    document_count INTEGER NOT NULL DEFAULT 0,
    l0_abstract    TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL DEFAULT ''
);

-- Dataset documents
CREATE TABLE IF NOT EXISTS dataset_documents (
    id                TEXT PRIMARY KEY,
    dataset_id        TEXT NOT NULL,
    name              TEXT NOT NULL,
    content           TEXT NOT NULL DEFAULT '',
    chunk_count       INTEGER NOT NULL DEFAULT 0,
    l0_abstract       TEXT NOT NULL DEFAULT '',
    l1_overview       TEXT NOT NULL DEFAULT '',
    processing_status TEXT NOT NULL DEFAULT 'pending',
    created_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dataset_documents_dataset_id ON dataset_documents(dataset_id);

-- Graph versions
CREATE TABLE IF NOT EXISTS graph_versions (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL,
    version      INTEGER NOT NULL,
    graph_def    TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    checksum     TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    published_at TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_graph_versions_agent_id ON graph_versions(agent_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_graph_versions_agent_version ON graph_versions(agent_id, version);

-- Provider configs
CREATE TABLE IF NOT EXISTS provider_configs (
    provider TEXT PRIMARY KEY,
    config   TEXT NOT NULL DEFAULT '{}'
);

-- Graph operations
CREATE TABLE IF NOT EXISTS graph_operations (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    node_id     TEXT,
    edge_from   TEXT,
    edge_to     TEXT,
    graph_def   TEXT,
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_graph_operations_agent_id ON graph_operations(agent_id);
CREATE INDEX IF NOT EXISTS idx_graph_operations_created_at ON graph_operations(created_at);

-- Kanban cards
CREATE TABLE IF NOT EXISTS kanban_cards (
    id              TEXT PRIMARY KEY,
    runtime_id      TEXT NOT NULL,
    type            TEXT NOT NULL DEFAULT 'task',
    status          TEXT NOT NULL DEFAULT 'pending',
    producer        TEXT NOT NULL DEFAULT '',
    consumer        TEXT NOT NULL DEFAULT '',
    target_agent_id TEXT NOT NULL DEFAULT '',
    query           TEXT NOT NULL DEFAULT '',
    output          TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    run_id          TEXT NOT NULL DEFAULT '',
    meta            TEXT NOT NULL DEFAULT '{}',
    payload         TEXT NOT NULL DEFAULT '{}',
    elapsed_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kanban_cards_runtime ON kanban_cards(runtime_id);
CREATE INDEX IF NOT EXISTS idx_kanban_cards_status ON kanban_cards(runtime_id, status);

-- +goose Down
DROP TABLE IF EXISTS kanban_cards;
DROP TABLE IF EXISTS graph_operations;
DROP TABLE IF EXISTS provider_configs;
DROP TABLE IF EXISTS graph_versions;
DROP TABLE IF EXISTS dataset_documents;
DROP TABLE IF EXISTS datasets;
DROP TABLE IF EXISTS execution_events;
DROP TABLE IF EXISTS workflow_runs;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS agents;
