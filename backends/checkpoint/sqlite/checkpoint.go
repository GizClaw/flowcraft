// Package sqlite provides a durable [agent.CheckpointStore] backed by
// SQLite. It implements the core store plus the optional
// [agent.CheckpointLister], [agent.CheckpointDeleter] and
// [agent.CheckpointJournal] interfaces.
//
// A record is one self-contained JSON envelope under its exec id. Writes are
// upserts; a per-row revision increments on every write of the base so
// concurrent callers observe a deterministic last-write-wins order even when
// wall clocks agree.
//
// Stores that take incremental writes keep them in a separate table: a base
// record plus appends ordered by a per-exec-id sequence. [Store.Load] replays
// them and returns the rebuilt record, so a reader never learns whether a
// checkpoint cost one row or a hundred. A gap in the chain is corruption and
// Load fails rather than returning partial state.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver.
)

const schema = `CREATE TABLE IF NOT EXISTS agent_checkpoints (
	exec_id    TEXT PRIMARY KEY,
	data       BLOB NOT NULL,
	revision   INTEGER NOT NULL DEFAULT 1,
	saved_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_checkpoint_deltas (
	exec_id    TEXT    NOT NULL,
	seq        INTEGER NOT NULL,
	data       BLOB    NOT NULL,
	saved_at   TEXT    NOT NULL,
	PRIMARY KEY (exec_id, seq)
) WITHOUT ROWID;`

// Store is a SQLite-backed [agent.CheckpointStore].
//
// Create one with [Open] (the store owns the database handle) or
// [New] (the caller keeps ownership of the handle). All methods are
// safe for concurrent use.
type Store struct {
	db        *sql.DB
	ownsDB    bool
	clock     func() time.Time
	maxDeltas int
}

type options struct {
	clock     func() time.Time
	maxDeltas int
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

// WithMaxDeltas caps how many incremental records may be appended to a base
// before [Store.Append] answers [agent.ErrNeedsBase], which makes the engine
// write a fresh base. Zero means no store-side cap: the writer's own compaction
// window still bounds the chain.
func WithMaxDeltas(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxDeltas = n
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
	return &Store{db: db, clock: o.clock, maxDeltas: o.maxDeltas}, nil
}

// Save implements [agent.CheckpointStore]. It behaves exactly like
// [Store.WriteBase]: the record replaces whatever was stored and any appended
// deltas are discarded, so a caller mixing the two write paths cannot leave a
// stale chain behind. Use [Store.Append] only through a
// [agent.CheckpointJournal] writer, which starts from a base.
func (s *Store) Save(ctx context.Context, cp agent.Checkpoint) error {
	return s.WriteBase(ctx, cp)
}

// WriteBase implements [agent.CheckpointJournal]: it stores a self-contained
// record and drops the delta chain for that execution.
func (s *Store) WriteBase(ctx context.Context, cp agent.Checkpoint) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite checkpoint: store is not initialized")
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	data, err := encodeJSON(cp)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: encode: %w", err)
	}
	savedAt := s.clock().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: save %s: %w", cp.ExecID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_checkpoints (exec_id, data, revision, saved_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(exec_id) DO UPDATE SET
			data = excluded.data,
			revision = agent_checkpoints.revision + 1,
			saved_at = excluded.saved_at
	`, cp.ExecID, data, savedAt); err != nil {
		return fmt.Errorf("sqlite checkpoint: save %s: %w", cp.ExecID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_checkpoint_deltas WHERE exec_id = ?`, cp.ExecID); err != nil {
		return fmt.Errorf("sqlite checkpoint: save %s: discard deltas: %w", cp.ExecID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite checkpoint: save %s: %w", cp.ExecID, err)
	}
	return nil
}

// Append implements [agent.CheckpointJournal]: it extends an existing record
// with one boundary.
//
// The base check, the chain cap and the insert are one statement, so an
// overlapping WriteBase or Delete cannot slip between them and have this delta
// land on a record it was not computed against. A statement that inserted
// nothing — no base, or at the cap — answers [agent.ErrNeedsBase], which tells
// the writer to store a full record instead.
//
// Ordering assumes one writer per exec id, which is what an engine gives: the
// sequence number is the row identity, so two writers racing on one record can
// collide and fail rather than interleave.
func (s *Store) Append(ctx context.Context, delta agent.CheckpointDelta) error {
	if s == nil || s.db == nil {
		return errors.New("sqlite checkpoint: store is not initialized")
	}
	if err := delta.Validate(); err != nil {
		return err
	}
	data, err := encodeJSON(delta)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: encode delta: %w", err)
	}
	savedAt := s.clock().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_checkpoint_deltas (exec_id, seq, data, saved_at)
		SELECT b.exec_id,
		       COALESCE((SELECT MAX(d.seq) FROM agent_checkpoint_deltas d
		                 WHERE d.exec_id = b.exec_id), 0) + 1,
		       ?, ?
		FROM agent_checkpoints b
		WHERE b.exec_id = ?
		  AND (? <= 0 OR (SELECT COUNT(*) FROM agent_checkpoint_deltas c
		                  WHERE c.exec_id = b.exec_id) < ?)
	`, data, savedAt, delta.ExecID, s.maxDeltas, s.maxDeltas)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: append %s: %w", delta.ExecID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: append %s: %w", delta.ExecID, err)
	}
	if inserted == 0 {
		return fmt.Errorf(
			"sqlite checkpoint: append %s: no base record, or the chain is at its cap: %w",
			delta.ExecID, agent.ErrNeedsBase)
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
	// Base and chain are read in one transaction: a WriteBase committing between
	// two separate reads would hand back a delta belonging to the new base
	// applied onto the old one — wrong state, reported as success.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: load %s: %w", execID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var data []byte
	err = tx.QueryRowContext(ctx,
		`SELECT data FROM agent_checkpoints WHERE exec_id = ?`, execID,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: load %s: %w", execID, err)
	}
	var cp agent.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: decode %s: %w", execID, err)
	}
	if err := cp.Validate(); err != nil {
		return nil, fmt.Errorf("sqlite checkpoint: corrupt %s: %w", execID, err)
	}
	if err := replayDeltas(ctx, tx, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// replayDeltas folds the incremental records onto a decoded base, in sequence
// order. A gap means the chain is corrupt: resuming from part of it would
// silently drop state, so it is reported as unavailable — the host's cue to
// start a fresh run instead.
func replayDeltas(ctx context.Context, tx *sql.Tx, cp *agent.Checkpoint) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT seq, data FROM agent_checkpoint_deltas
		WHERE exec_id = ? ORDER BY seq
	`, cp.ExecID)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: load %s: replay: %w", cp.ExecID, err)
	}
	defer func() { _ = rows.Close() }()

	want := int64(1)
	for rows.Next() {
		var seq int64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			return fmt.Errorf("sqlite checkpoint: load %s: replay: %w", cp.ExecID, err)
		}
		if seq != want {
			return errdefs.NotAvailablef(
				"sqlite checkpoint: load %s: delta chain is broken (want seq %d, got %d)",
				cp.ExecID, want, seq)
		}
		var delta agent.CheckpointDelta
		if err := json.Unmarshal(data, &delta); err != nil {
			return fmt.Errorf("sqlite checkpoint: corrupt %s delta %d: %w", cp.ExecID, seq, err)
		}
		if err := delta.Validate(); err != nil {
			return fmt.Errorf("sqlite checkpoint: corrupt %s delta %d: %w", cp.ExecID, seq, err)
		}
		delta.ApplyTo(cp)
		want++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite checkpoint: load %s: replay: %w", cp.ExecID, err)
	}
	return nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite checkpoint: delete %s: %w", execID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_checkpoint_deltas WHERE exec_id = ?`, execID); err != nil {
		return fmt.Errorf("sqlite checkpoint: delete %s: %w", execID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agent_checkpoints WHERE exec_id = ?`, execID); err != nil {
		return fmt.Errorf("sqlite checkpoint: delete %s: %w", execID, err)
	}
	if err := tx.Commit(); err != nil {
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
	_ agent.CheckpointJournal = (*Store)(nil)
)

// encodeJSON returns the column value for v in one copy.
//
// json.Marshal hands back the encoder's own buffer, so binding it directly
// avoids the second copy a TEXT column forced (string(data)); an intermediate
// bytes.Buffer only adds a third. A board payload is the largest thing this
// store writes, so the copies are what the encode costs.
func encodeJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
