package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"

	_ "modernc.org/sqlite"
)

// Store persists [engine.Checkpoint] records in a SQLite database.
// One row per ExecID; Save UPSERTs the latest snapshot. Safe for
// concurrent use across goroutines (database/sql handles pooling).
type Store struct {
	db        *sql.DB
	ownsDB    bool
	tableName string
}

// Option configures [Open] / [Wrap].
type Option func(*config)

type config struct {
	tableName string
}

// WithTable overrides the table name (default "engine_checkpoints").
// Useful for multi-tenant deployments that namespace tables per
// vessel.
func WithTable(name string) Option {
	return func(c *config) { c.tableName = name }
}

func defaultConfig() config {
	return config{tableName: "engine_checkpoints"}
}

// Open creates a Store backed by a fresh database/sql connection
// pool against dsn. The Store takes ownership of the pool — closing
// the Store closes the pool. Migrations are run synchronously before
// Open returns.
//
// dsn is passed verbatim to modernc.org/sqlite; common forms:
//   - "file:checkpoints.db?_pragma=journal_mode(WAL)" — on-disk WAL
//   - "file::memory:?cache=shared" — in-memory shared (for tests)
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: open %q: %w", dsn, err)
	}
	// SQLite serialises writes at the file level. Limit the database/sql
	// pool to a single connection so writers queue inside Go instead of
	// hitting SQLITE_BUSY. Reads still run on this connection — fine for
	// the single-process daemons this backend targets.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite checkpoint: ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite checkpoint: set busy_timeout: %w", err)
	}

	s := newStore(db, true, opts...)
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Wrap builds a Store on top of an existing *sql.DB. The caller
// retains ownership of db; closing the Store does not close the
// underlying pool. Migrate must be called before any Save / Load
// when callers use Wrap.
func Wrap(db *sql.DB, opts ...Option) *Store {
	return newStore(db, false, opts...)
}

func newStore(db *sql.DB, owns bool, opts ...Option) *Store {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Store{db: db, ownsDB: owns, tableName: cfg.tableName}
}

// Close releases resources owned by the Store. Safe to call
// repeatedly; only the first call has an effect when the Store
// owns its DB.
func (s *Store) Close() error {
	if s == nil || !s.ownsDB || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Migrate creates the checkpoint table if it does not yet exist.
// Run automatically by [Open]; expose for callers that build the
// Store via [Wrap].
func (s *Store) Migrate(ctx context.Context) error {
	stmt := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    exec_id    TEXT PRIMARY KEY,
    step       TEXT NOT NULL DEFAULT '',
    iteration  INTEGER NOT NULL DEFAULT 0,
    board      BLOB NOT NULL,
    payload    BLOB,
    attributes BLOB,
    ts         INTEGER NOT NULL
)`, s.tableName)
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("sqlite checkpoint: migrate: %w", err)
	}
	return nil
}

// Save persists cp, overwriting any existing record with the same
// ExecID.
func (s *Store) Save(ctx context.Context, cp engine.Checkpoint) error {
	if cp.ExecID == "" {
		return errors.New("sqlite checkpoint: Save: empty ExecID")
	}
	if cp.Board == nil {
		return errors.New("sqlite checkpoint: Save: nil Board")
	}

	boardJSON, err := json.Marshal(cp.Board)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: marshal Board: %w", err)
	}
	var attrJSON []byte
	if len(cp.Attributes) > 0 {
		attrJSON, err = json.Marshal(cp.Attributes)
		if err != nil {
			return fmt.Errorf("sqlite checkpoint: marshal Attributes: %w", err)
		}
	}
	ts := cp.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	stmt := fmt.Sprintf(`
INSERT INTO %s (exec_id, step, iteration, board, payload, attributes, ts)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(exec_id) DO UPDATE SET
    step       = excluded.step,
    iteration  = excluded.iteration,
    board      = excluded.board,
    payload    = excluded.payload,
    attributes = excluded.attributes,
    ts         = excluded.ts
`, s.tableName)

	_, err = s.db.ExecContext(ctx, stmt,
		cp.ExecID, cp.Step, cp.Iteration,
		boardJSON, []byte(cp.Payload), attrJSON, ts.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: Save: %w", err)
	}
	return nil
}

// Load returns the latest checkpoint for execID, or (nil, nil) if
// none exists.
func (s *Store) Load(ctx context.Context, execID string) (*engine.Checkpoint, error) {
	stmt := fmt.Sprintf(`
SELECT exec_id, step, iteration, board, payload, attributes, ts
FROM %s WHERE exec_id = ?
`, s.tableName)

	row := s.db.QueryRowContext(ctx, stmt, execID)
	var (
		gotID    string
		step     string
		iter     int
		board    []byte
		payload  []byte
		attrJSON []byte
		tsMilli  int64
	)
	if err := row.Scan(&gotID, &step, &iter, &board, &payload, &attrJSON, &tsMilli); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite checkpoint: Load: %w", err)
	}

	var snap engine.BoardSnapshot
	if err := json.Unmarshal(board, &snap); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: unmarshal Board: %w", err)
	}
	var attrs map[string]string
	if len(attrJSON) > 0 {
		if err := json.Unmarshal(attrJSON, &attrs); err != nil {
			return nil, fmt.Errorf("sqlite checkpoint: unmarshal Attributes: %w", err)
		}
	}
	cp := &engine.Checkpoint{
		ExecID:     gotID,
		Step:       step,
		Iteration:  iter,
		Board:      &snap,
		Payload:    json.RawMessage(payload),
		Attributes: attrs,
		Timestamp:  time.UnixMilli(tsMilli).UTC(),
	}
	if len(cp.Payload) == 0 {
		cp.Payload = nil
	}
	return cp, nil
}

// List enumerates persisted exec ids. Implements
// [engine.CheckpointLister].
func (s *Store) List(ctx context.Context) ([]string, error) {
	stmt := fmt.Sprintf(`SELECT exec_id FROM %s ORDER BY ts DESC`, s.tableName)
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: List: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite checkpoint: List scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: List rows: %w", err)
	}
	return ids, nil
}

// Delete removes the checkpoint for execID. Missing exec ids are a
// no-op (no error). Implements [engine.CheckpointDeleter].
func (s *Store) Delete(ctx context.Context, execID string) error {
	stmt := fmt.Sprintf(`DELETE FROM %s WHERE exec_id = ?`, s.tableName)
	if _, err := s.db.ExecContext(ctx, stmt, execID); err != nil {
		return fmt.Errorf("sqlite checkpoint: Delete: %w", err)
	}
	return nil
}

var (
	_ engine.CheckpointStore   = (*Store)(nil)
	_ engine.CheckpointLister  = (*Store)(nil)
	_ engine.CheckpointDeleter = (*Store)(nil)
)
