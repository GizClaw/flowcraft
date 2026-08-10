// Package sqlite provides a durable [agent.CheckpointStore] backed by
// SQLite. It implements the core store plus the optional
// [agent.CheckpointLister] and [agent.CheckpointDeleter] interfaces.
//
// Each checkpoint is stored as a single JSON envelope under its exec
// id. Writes are upserts; a per-row revision increments on every Save
// so concurrent callers observe a deterministic last-write-wins
// order even when wall clocks agree.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver.
)

const schema = `CREATE TABLE IF NOT EXISTS agent_checkpoints (
	exec_id    TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	revision   INTEGER NOT NULL DEFAULT 1,
	saved_at   TEXT NOT NULL
);`

// Store is a SQLite-backed [agent.CheckpointStore].
//
// Create one with [Open] (the store owns the database handle) or
// [New] (the caller keeps ownership of the handle). All methods are
// safe for concurrent use.
type Store struct {
	db     *sql.DB
	ownsDB bool
	clock  func() time.Time
}

type options struct {
	clock func() time.Time
}

// Option configures a Store.
type Option func(*options)

// WithClock overrides the clock used for the store's saved_at marker.
// It is primarily useful in tests.
func WithClock(clock func() time.Time) Option {
	return func(o *options) {
		if clock != nil {
			o.clock = clock
		}
	}
}

// Open opens (creating if necessary) the SQLite database at path and
// returns a Store that owns the handle. The store enables WAL and a
// busy timeout, and serializes access through a single connection so
// in-memory databases ("file::memory:?" and ":memory:") behave like
// durable ones.
func Open(path string, opts ...Option) (*Store, error) {
	return OpenContext(context.Background(), path, opts...)
}

// OpenContext is [Open] with an explicit build context for the
// initialization PRAGMAs and schema migration.
func OpenContext(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite checkpoint: path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite checkpoint: busy timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite checkpoint: journal mode: %w", err)
	}
	store, err := NewContext(ctx, db, opts...)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// New wraps an existing SQLite handle. The caller keeps ownership of
// db; [Store.Close] is a no-op for stores created with New.
func New(db *sql.DB, opts ...Option) (*Store, error) {
	return NewContext(context.Background(), db, opts...)
}

// NewContext is [New] with an explicit migration context.
func NewContext(ctx context.Context, db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("sqlite checkpoint: db is required")
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: migrate: %w", err)
	}
	o := options{clock: time.Now}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return &Store{db: db, clock: o.clock}, nil
}

// Save implements [agent.CheckpointStore].
func (s *Store) Save(ctx context.Context, cp agent.Checkpoint) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite checkpoint: store is not initialized")
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: encode: %w", err)
	}
	savedAt := s.clock().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_checkpoints (exec_id, data, revision, saved_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(exec_id) DO UPDATE SET
			data = excluded.data,
			revision = agent_checkpoints.revision + 1,
			saved_at = excluded.saved_at
	`, cp.ExecID, string(data), savedAt)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: save %s: %w", cp.ExecID, err)
	}
	return nil
}

// Load implements [agent.CheckpointStore].
func (s *Store) Load(ctx context.Context, execID string) (*agent.Checkpoint, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("sqlite checkpoint: store is not initialized")
	}
	if strings.TrimSpace(execID) == "" {
		return nil, errdefs.Validation(errors.New("sqlite checkpoint: exec_id is required"))
	}
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM agent_checkpoints WHERE exec_id = ?`, execID,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: load %s: %w", execID, err)
	}
	var cp agent.Checkpoint
	if err := json.Unmarshal([]byte(data), &cp); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: decode %s: %w", execID, err)
	}
	if err := cp.Validate(); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: corrupt %s: %w", execID, err)
	}
	return &cp, nil
}

// List implements [agent.CheckpointLister].
func (s *Store) List(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("sqlite checkpoint: store is not initialized")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT exec_id FROM agent_checkpoints ORDER BY exec_id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite checkpoint: list: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: list: %w", err)
	}
	return ids, nil
}

// Delete implements [agent.CheckpointDeleter].
func (s *Store) Delete(ctx context.Context, execID string) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite checkpoint: store is not initialized")
	}
	if strings.TrimSpace(execID) == "" {
		return errdefs.Validation(errors.New("sqlite checkpoint: exec_id is required"))
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_checkpoints WHERE exec_id = ?`, execID); err != nil {
		return fmt.Errorf("sqlite checkpoint: delete %s: %w", execID, err)
	}
	return nil
}

// Close closes the database handle when the store owns it. Stores
// created with [New] leave the caller's handle open.
func (s *Store) Close() error {
	if s == nil || s.db == nil || !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// Compile-time assertions that Store satisfies the checkpoint store
// contract plus the optional extensions.
var (
	_ agent.CheckpointStore   = (*Store)(nil)
	_ agent.CheckpointLister  = (*Store)(nil)
	_ agent.CheckpointDeleter = (*Store)(nil)
)
