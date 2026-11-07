// Package checkpoint stores agent tasks on the local filesystem with SQLite.
//
// The schema is two tables plus a blob table, and it is written to translate:
//
//	tasks   one row per Task: the immutable seed, the mutable projection, a version
//	events  the appended facts, one row per fact, keyed (task_id, seq)
//	blobs   artifact bytes by content hash
//
// Every save is one transaction: append the round's events and update the
// projection together, so a reader never sees a task that mentions a message the
// event log does not have. Not persisting partial rounds is the same rule the
// session services in the ecosystem converged on.
//
// The types are SQLite's, but the shapes are portable: TEXT stays TEXT, BLOB
// becomes BYTEA, INTEGER stays INTEGER, and the only upsert uses the standard
// ON CONFLICT form.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	task_id    TEXT PRIMARY KEY,
	context_id TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL,
	round      INTEGER NOT NULL DEFAULT 0,
	schema     INTEGER NOT NULL,
	runtime    TEXT NOT NULL,
	seed           BLOB NOT NULL DEFAULT '',
	body           BLOB NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1,
	server_version INTEGER NOT NULL DEFAULT 0,
	updated_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	task_id TEXT    NOT NULL,
	seq     INTEGER NOT NULL,
	kind    TEXT    NOT NULL,
	payload BLOB    NOT NULL,
	PRIMARY KEY (task_id, seq)
);
CREATE TABLE IF NOT EXISTS blobs (
	ref  TEXT PRIMARY KEY,
	body BLOB NOT NULL
);`

// SQLite is the database/sql backend. A Postgres backend is a sibling file
// with the same shape, which is why nothing here is named "local".
type SQLite struct {
	db *sql.DB
}

var _ agent.Store = (*SQLite)(nil)

// Open prepares a SQLite store at path. ":memory:" is accepted for tests.
func Open(path string) (*SQLite, error) {
	if path == "" {
		return nil, errdefs.Validationf("checkpoint: a database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: open: %w", err)
	}
	// One writer. The pragmas below apply per connection, and two connections
	// writing at once return SQLITE_BUSY even with a busy timeout set on only one
	// of them. A local checkpoint store gains nothing from a pool.
	db.SetMaxOpenConns(1)
	// Foreign keys are off by default in SQLite, and a cascade that silently
	// does not fire leaves events behind a deleted task. WAL keeps a reader from
	// blocking the loop's writes.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("checkpoint: %s: %w", pragma, err)
		}
	}
	// An older database predates the adapter's own counter; adding it here keeps
	// one migration path without a schema-version table.
	_, _ = db.Exec("ALTER TABLE tasks ADD COLUMN server_version INTEGER NOT NULL DEFAULT 0")
	// A database written before the projection stopped carrying history has no
	// seed column; Load falls back to the body for those rows.
	_, _ = db.Exec("ALTER TABLE tasks ADD COLUMN seed BLOB NOT NULL DEFAULT ''")
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checkpoint: migrate: %w", err)
	}
	return &SQLite{db: db}, nil
}

// Close releases the database handle.
func (l *SQLite) Close() error { return l.db.Close() }

// Save updates the projection and appends the round's events in one transaction.
func (l *SQLite) Save(ctx context.Context, checkpoint agent.Checkpoint, events []agent.EventRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkpoint.Task.ID == "" {
		return errdefs.Validationf("checkpoint: task id is required")
	}
	// The seed is the task as of the first save and never changes; the body holds
	// only what changes after it. Writing history here made every save rewrite the
	// whole task, so n rounds cost O(n^2) bytes.
	seed, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("checkpoint: encode seed: %w", err)
	}
	body, err := json.Marshal(struct {
		Artifacts []agent.Artifact  `json:"artifacts,omitempty"`
		Pending   *message.ToolCall `json:"pending,omitempty"`
	}{checkpoint.Task.Artifacts, checkpoint.Task.Pending})
	if err != nil {
		return fmt.Errorf("checkpoint: encode projection: %w", err)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("checkpoint: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (task_id, context_id, state, round, schema, runtime, seed, body, version, server_version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			context_id     = excluded.context_id,
			state          = excluded.state,
			round          = excluded.round,
			schema         = excluded.schema,
			runtime        = excluded.runtime,
			body           = excluded.body,
			version        = tasks.version + 1,
			server_version = excluded.server_version,
			updated_at     = excluded.updated_at`,
		checkpoint.Task.ID,
		checkpoint.Task.ContextID,
		string(checkpoint.Task.State),
		checkpoint.Task.Round,
		checkpoint.Schema,
		string(checkpoint.Runtime),
		seed,
		body,
		checkpoint.ServerVersion,
		now,
	); err != nil {
		return fmt.Errorf("checkpoint: project task: %w", err)
	}

	if len(events) > 0 {
		var next int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE task_id = ?`,
			checkpoint.Task.ID,
		).Scan(&next); err != nil {
			return fmt.Errorf("checkpoint: next sequence: %w", err)
		}
		for i, event := range events {
			if event.Kind == "" {
				return errdefs.Validationf("checkpoint: event %d has no kind", i)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (task_id, seq, kind, payload) VALUES (?, ?, ?, ?)`,
				checkpoint.Task.ID, next+int64(i), event.Kind, []byte(event.Payload),
			); err != nil {
				return fmt.Errorf("checkpoint: append event: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("checkpoint: commit: %w", err)
	}
	return nil
}

// Load reconstructs one task: the immutable seed plus the event log, which is the
// single source of history after the seed.
func (l *SQLite) Load(ctx context.Context, taskID string) (agent.Checkpoint, error) {
	var (
		contextID, state string
		round            int64
		seed, body       []byte
		version          int64
		serverVersion    int64
	)
	err := l.db.QueryRowContext(ctx,
		`SELECT context_id, state, round, seed, body, version, server_version
		 FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&contextID, &state, &round, &seed, &body, &version, &serverVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	if err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: load task: %w", err)
	}

	var checkpoint agent.Checkpoint
	if len(seed) == 0 {
		// A row written before the seed column existed carried the whole task.
		if err := json.Unmarshal(body, &checkpoint); err != nil {
			return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode task: %w", err)
		}
	} else {
		if err := json.Unmarshal(seed, &checkpoint); err != nil {
			return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode seed: %w", err)
		}
		checkpoint.Task.ContextID = contextID
		checkpoint.Task.State = agent.TaskState(state)
		checkpoint.Task.Round = int(round)
		var mutable struct {
			Artifacts []agent.Artifact  `json:"artifacts,omitempty"`
			Pending   *message.ToolCall `json:"pending,omitempty"`
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &mutable); err != nil {
				return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode projection: %w", err)
			}
		}
		checkpoint.Task.Artifacts = mutable.Artifacts
		checkpoint.Task.Pending = mutable.Pending
		if err := l.appendEvents(ctx, &checkpoint); err != nil {
			return agent.Checkpoint{}, err
		}
	}
	checkpoint.Version = version
	checkpoint.ServerVersion = serverVersion
	return checkpoint, nil
}

// appendEvents replays the log onto the seed. Kinds other than "message" carry
// protocol bookkeeping, not history, and are skipped.
func (l *SQLite) appendEvents(ctx context.Context, checkpoint *agent.Checkpoint) error {
	rows, err := l.db.QueryContext(ctx,
		`SELECT kind, payload FROM events WHERE task_id = ? ORDER BY seq`,
		checkpoint.Task.ID)
	if err != nil {
		return fmt.Errorf("checkpoint: load events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			kind    string
			payload []byte
		)
		if err := rows.Scan(&kind, &payload); err != nil {
			return fmt.Errorf("checkpoint: scan event: %w", err)
		}
		if kind != "message" {
			continue
		}
		var msg message.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			// One record the loop cannot represent must not make the task
			// unreadable; the same rule governs tasks/get on the wire.
			continue
		}
		checkpoint.Task.History = append(checkpoint.Task.History, msg)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checkpoint: load events: %w", err)
	}
	return nil
}

// LatestForContext returns the newest task of one conversation.
func (l *SQLite) LatestForContext(ctx context.Context, conversationID string) (agent.Checkpoint, error) {
	var taskID string
	err := l.db.QueryRowContext(ctx,
		`SELECT task_id FROM tasks WHERE context_id = ?
		 ORDER BY updated_at DESC LIMIT 1`, conversationID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	if err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: latest for context: %w", err)
	}
	return l.Load(ctx, taskID)
}

// Events returns the appended facts of one task, oldest first.
func (l *SQLite) Events(ctx context.Context, taskID string) ([]agent.EventRecord, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT seq, kind, payload FROM events WHERE task_id = ? ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]agent.EventRecord, 0, 16)
	for rows.Next() {
		var record agent.EventRecord
		var payload []byte
		if err := rows.Scan(&record.Seq, &record.Kind, &payload); err != nil {
			return nil, fmt.Errorf("checkpoint: scan event: %w", err)
		}
		record.Payload = payload
		out = append(out, record)
	}
	return out, rows.Err()
}

// List returns every task, newest first.
func (l *SQLite) List(ctx context.Context) ([]agent.TaskSummary, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT task_id, context_id, state, round, updated_at FROM tasks ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]agent.TaskSummary, 0, 16)
	for rows.Next() {
		var summary agent.TaskSummary
		if err := rows.Scan(&summary.ID, &summary.ContextID, &summary.State,
			&summary.Round, &summary.UpdatedAt); err != nil {
			return nil, fmt.Errorf("checkpoint: scan summary: %w", err)
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// Delete removes a task and its events.
func (l *SQLite) Delete(ctx context.Context, taskID string) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM events WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("checkpoint: delete events: %w", err)
	}
	if _, err := l.db.ExecContext(ctx, `DELETE FROM tasks WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("checkpoint: delete task: %w", err)
	}
	return nil
}

// PutBlob stores bytes under their content hash.
func (l *SQLite) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	ref := hex.EncodeToString(sum[:])
	if _, err := l.db.ExecContext(ctx,
		`INSERT INTO blobs (ref, body) VALUES (?, ?) ON CONFLICT(ref) DO NOTHING`,
		ref, data,
	); err != nil {
		return "", fmt.Errorf("checkpoint: store blob: %w", err)
	}
	return ref, nil
}

// GetBlob returns the bytes a reference names.
func (l *SQLite) GetBlob(ctx context.Context, ref string) ([]byte, error) {
	if len(ref) != sha256.Size*2 {
		return nil, errdefs.Validationf("checkpoint: %q is not a blob reference", ref)
	}
	var body []byte
	err := l.db.QueryRowContext(ctx, `SELECT body FROM blobs WHERE ref = ?`, ref).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("checkpoint: blob %s: %w", ref, agent.ErrNoCheckpoint)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read blob: %w", err)
	}
	return body, nil
}
