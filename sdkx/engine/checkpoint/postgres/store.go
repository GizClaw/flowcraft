package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists [engine.Checkpoint] records in a PostgreSQL table.
// One row per ExecID; Save UPSERTs the latest snapshot. Safe for
// concurrent use across goroutines and processes (the unique
// constraint on exec_id makes the UPSERT idempotent).
type Store struct {
	pool       *pgxpool.Pool
	ownsPool   bool
	tableName  string
	migrateRun bool
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

// Open creates a Store backed by a fresh pgxpool.Pool against dsn.
// The Store takes ownership of the pool — closing the Store closes
// the pool. Migrations are run synchronously before Open returns.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres checkpoint: pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres checkpoint: ping: %w", err)
	}
	s := newStore(pool, true, opts...)
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Wrap builds a Store on top of an existing pgxpool.Pool. The caller
// retains ownership of pool; closing the Store does not close it.
// Migrate must be called before any Save / Load when callers use Wrap.
func Wrap(pool *pgxpool.Pool, opts ...Option) *Store {
	return newStore(pool, false, opts...)
}

func newStore(pool *pgxpool.Pool, owns bool, opts ...Option) *Store {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Store{pool: pool, ownsPool: owns, tableName: cfg.tableName}
}

// Close releases resources owned by the Store. Safe to call
// repeatedly; only the first call has an effect when the Store
// owns its pool.
func (s *Store) Close() error {
	if s == nil || !s.ownsPool || s.pool == nil {
		return nil
	}
	s.pool.Close()
	s.pool = nil
	return nil
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
    board      JSONB NOT NULL,
    payload    JSONB,
    attributes JSONB,
    ts         TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`, s.tableName)
	if _, err := s.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("postgres checkpoint: migrate: %w", err)
	}
	s.migrateRun = true
	return nil
}

// Save persists cp, overwriting any existing record with the same
// ExecID.
func (s *Store) Save(ctx context.Context, cp engine.Checkpoint) error {
	if cp.ExecID == "" {
		return errors.New("postgres checkpoint: Save: empty ExecID")
	}
	if cp.Board == nil {
		return errors.New("postgres checkpoint: Save: nil Board")
	}

	boardJSON, err := json.Marshal(cp.Board)
	if err != nil {
		return fmt.Errorf("postgres checkpoint: marshal Board: %w", err)
	}
	var payload []byte
	if len(cp.Payload) > 0 {
		payload = []byte(cp.Payload)
	}
	var attrJSON []byte
	if len(cp.Attributes) > 0 {
		attrJSON, err = json.Marshal(cp.Attributes)
		if err != nil {
			return fmt.Errorf("postgres checkpoint: marshal Attributes: %w", err)
		}
	}
	ts := cp.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	stmt := fmt.Sprintf(`
INSERT INTO %s (exec_id, step, iteration, board, payload, attributes, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (exec_id) DO UPDATE SET
    step       = EXCLUDED.step,
    iteration  = EXCLUDED.iteration,
    board      = EXCLUDED.board,
    payload    = EXCLUDED.payload,
    attributes = EXCLUDED.attributes,
    ts         = EXCLUDED.ts
`, s.tableName)

	_, err = s.pool.Exec(ctx, stmt,
		cp.ExecID, cp.Step, cp.Iteration,
		boardJSON, payload, attrJSON, ts,
	)
	if err != nil {
		return fmt.Errorf("postgres checkpoint: Save: %w", err)
	}
	return nil
}

// Load returns the latest checkpoint for execID, or (nil, nil) if
// none exists.
func (s *Store) Load(ctx context.Context, execID string) (*engine.Checkpoint, error) {
	stmt := fmt.Sprintf(`
SELECT exec_id, step, iteration, board, payload, attributes, ts
FROM %s WHERE exec_id = $1
`, s.tableName)

	var (
		gotID    string
		step     string
		iter     int
		board    []byte
		payload  []byte
		attrJSON []byte
		ts       time.Time
	)
	err := s.pool.QueryRow(ctx, stmt, execID).Scan(
		&gotID, &step, &iter, &board, &payload, &attrJSON, &ts,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres checkpoint: Load: %w", err)
	}

	var snap engine.BoardSnapshot
	if err := json.Unmarshal(board, &snap); err != nil {
		return nil, fmt.Errorf("postgres checkpoint: unmarshal Board: %w", err)
	}
	var attrs map[string]string
	if len(attrJSON) > 0 {
		if err := json.Unmarshal(attrJSON, &attrs); err != nil {
			return nil, fmt.Errorf("postgres checkpoint: unmarshal Attributes: %w", err)
		}
	}
	cp := &engine.Checkpoint{
		ExecID:     gotID,
		Step:       step,
		Iteration:  iter,
		Board:      &snap,
		Payload:    json.RawMessage(payload),
		Attributes: attrs,
		Timestamp:  ts.UTC(),
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
	rows, err := s.pool.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("postgres checkpoint: List: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres checkpoint: List scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres checkpoint: List rows: %w", err)
	}
	return ids, nil
}

// Delete removes the checkpoint for execID. Missing exec ids are a
// no-op (no error). Implements [engine.CheckpointDeleter].
func (s *Store) Delete(ctx context.Context, execID string) error {
	stmt := fmt.Sprintf(`DELETE FROM %s WHERE exec_id = $1`, s.tableName)
	if _, err := s.pool.Exec(ctx, stmt, execID); err != nil {
		return fmt.Errorf("postgres checkpoint: Delete: %w", err)
	}
	return nil
}

var (
	_ engine.CheckpointStore   = (*Store)(nil)
	_ engine.CheckpointLister  = (*Store)(nil)
	_ engine.CheckpointDeleter = (*Store)(nil)
)
