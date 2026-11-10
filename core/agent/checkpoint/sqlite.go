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
func (l *SQLite) Save(ctx context.Context, checkpoint agent.Checkpoint, events []agent.EventRecord) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if checkpoint.Task.ID == "" {
		return 0, 0, errdefs.Validationf("checkpoint: task id is required")
	}
	// The projection never carries history: the event log is its only source, so
	// serializing a reconstructed transcript here would be wasted bytes on every
	// save.
	checkpoint.Task.History = nil
	// The seed is the task as of the first save and never changes; the body holds
	// only what changes after it. Writing history here made every save rewrite the
	// whole task, so n rounds cost O(n^2) bytes.
	seed, err := json.Marshal(checkpoint)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: encode seed: %w", err)
	}
	// The body is the mutable part of the row: the projection plus whatever an
	// adapter above the agent keeps in Sections. History is not here; it lives in
	// the event log.
	body, err := json.Marshal(struct {
		Artifacts []agent.Artifact           `json:"artifacts,omitempty"`
		Pending   *message.ToolCall          `json:"pending,omitempty"`
		Sections  map[string]json.RawMessage `json:"sections,omitempty"`
	}{checkpoint.Task.Artifacts, checkpoint.Task.Pending, checkpoint.Sections})
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: encode projection: %w", err)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: begin: %w", err)
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
		checkpoint.Task.SessionID,
		string(checkpoint.Task.State),
		checkpoint.Task.Round,
		checkpoint.Schema,
		string(checkpoint.Runtime),
		seed,
		body,
		checkpoint.ServerVersion,
		now,
	); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: project task: %w", err)
	}

	var first, last int64
	if len(events) > 0 {
		var next int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE task_id = ?`,
			checkpoint.Task.ID,
		).Scan(&next); err != nil {
			return 0, 0, fmt.Errorf("checkpoint: next sequence: %w", err)
		}
		first, last = next, next+int64(len(events))-1
		for i, event := range events {
			if event.Kind == "" {
				return 0, 0, errdefs.Validationf("checkpoint: event %d has no kind", i)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (task_id, seq, kind, payload) VALUES (?, ?, ?, ?)`,
				checkpoint.Task.ID, next+int64(i), event.Kind, []byte(event.Payload),
			); err != nil {
				return 0, 0, fmt.Errorf("checkpoint: append event: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: commit: %w", err)
	}
	return first, last, nil
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
		checkpoint.Task.SessionID = contextID
		checkpoint.Task.State = agent.TaskState(state)
		checkpoint.Task.Round = int(round)
		var mutable struct {
			Artifacts []agent.Artifact           `json:"artifacts,omitempty"`
			Pending   *message.ToolCall          `json:"pending,omitempty"`
			Sections  map[string]json.RawMessage `json:"sections,omitempty"`
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &mutable); err != nil {
				return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode projection: %w", err)
			}
		}
		checkpoint.Task.Artifacts = mutable.Artifacts
		checkpoint.Task.Pending = mutable.Pending
		// The seed holds the sections of the first save; later saves update them
		// through the projection. An adapter that stops writing a section keeps
		// its last value, which is what "owner-keyed" means.
		if len(mutable.Sections) > 0 {
			if checkpoint.Sections == nil {
				checkpoint.Sections = make(map[string]json.RawMessage, len(mutable.Sections))
			}
			for key, value := range mutable.Sections {
				checkpoint.Sections[key] = value
			}
		}
	}
	// History is the event log's, not the projection's.
	checkpoint.Task.History = nil
	checkpoint.Version = version
	checkpoint.ServerVersion = serverVersion
	return checkpoint, nil
}

// LatestForSession returns the newest task of one session.
func (l *SQLite) LatestForSession(ctx context.Context, sessionID string) (agent.Checkpoint, error) {
	var taskID string
	err := l.db.QueryRowContext(ctx,
		`SELECT task_id FROM tasks WHERE context_id = ?
		 ORDER BY updated_at DESC LIMIT 1`, sessionID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	if err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: latest for context: %w", err)
	}
	return l.Load(ctx, taskID)
}

// Events returns every appended fact of one task, oldest first.
func (l *SQLite) Events(ctx context.Context, taskID string) ([]agent.EventRecord, error) {
	return l.EventsAfter(ctx, taskID, 0)
}

// EventsAfter returns the facts recorded after seq, oldest first.
func (l *SQLite) EventsAfter(ctx context.Context, taskID string, after int64) ([]agent.EventRecord, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT seq, kind, payload FROM events WHERE task_id = ? AND seq > ? ORDER BY seq`,
		taskID, after)
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

// SetServerVersion advances the adapter's counter, leaving the projection and
// the store's own version alone: nothing the engine owns changed.
func (l *SQLite) SetServerVersion(ctx context.Context, taskID string, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := l.db.ExecContext(ctx,
		`UPDATE tasks SET server_version = ?, updated_at = ? WHERE task_id = ?`,
		version, time.Now().UTC().Format(time.RFC3339Nano), taskID)
	if err != nil {
		return fmt.Errorf("checkpoint: set server version: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checkpoint: set server version: %w", err)
	}
	if affected == 0 {
		return agent.ErrNoCheckpoint
	}
	return nil
}

// TruncateEvents removes the events at or before seq, keeping the tail.
func (l *SQLite) TruncateEvents(ctx context.Context, taskID string, seq int64) error {
	if seq < 0 {
		return errdefs.Validationf("checkpoint: truncation point must not be negative")
	}
	var newest int64
	if err := l.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE task_id = ?`, taskID,
	).Scan(&newest); err != nil {
		return fmt.Errorf("checkpoint: latest sequence: %w", err)
	}
	if newest == 0 {
		return nil // nothing recorded
	}
	if seq >= newest {
		// Removing the tail would reset MAX(seq)+1 and let a later save reuse an
		// identifier that a snapshot may still name.
		return errdefs.Validationf(
			"checkpoint: truncating through %d would remove the newest event (%d)", seq, newest)
	}
	if _, err := l.db.ExecContext(ctx,
		`DELETE FROM events WHERE task_id = ? AND seq <= ?`, taskID, seq); err != nil {
		return fmt.Errorf("checkpoint: truncate events: %w", err)
	}
	return nil
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
		if err := rows.Scan(&summary.ID, &summary.SessionID, &summary.State,
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
