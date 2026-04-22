package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"

	_ "modernc.org/sqlite"
)

// SQLiteJobQueue is the persistent recall.JobQueue.
type SQLiteJobQueue struct {
	db *sql.DB

	mu  sync.Mutex
	seq uint64
}

// Open opens (or creates) the queue file and runs migrations + crash recovery.
func Open(path string) (*SQLiteJobQueue, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite jobqueue: pragma %q: %w", p, err)
		}
	}
	q := &SQLiteJobQueue{db: db}
	if err := q.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := q.recoverRunning(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return q, nil
}

// Close implements recall.JobQueue.
func (q *SQLiteJobQueue) Close() error { return q.db.Close() }

func (q *SQLiteJobQueue) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_jobs (
			id           TEXT PRIMARY KEY,
			namespace    TEXT NOT NULL,
			payload      BLOB NOT NULL,
			state        TEXT NOT NULL,
			attempts     INTEGER NOT NULL DEFAULT 0,
			last_error   TEXT,
			entry_ids    TEXT,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			next_run_at  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS memory_jobs_pending ON memory_jobs(state, next_run_at)`,
	}
	for _, s := range stmts {
		if _, err := q.db.Exec(s); err != nil {
			return fmt.Errorf("sqlite jobqueue: migrate: %w", err)
		}
	}
	return nil
}

// recoverRunning resets stranded running jobs to pending.
func (q *SQLiteJobQueue) recoverRunning() error {
	now := time.Now().UnixMilli()
	_, err := q.db.Exec(
		`UPDATE memory_jobs SET state='pending', next_run_at=?, updated_at=? WHERE state='running'`,
		now, now,
	)
	return err
}

// Enqueue implements recall.JobQueue.
func (q *SQLiteJobQueue) Enqueue(ctx context.Context, namespace string, p recall.JobPayload) (recall.JobID, error) {
	id := q.newID()
	bytes, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	if _, err := q.db.ExecContext(ctx,
		`INSERT INTO memory_jobs (id,namespace,payload,state,attempts,created_at,updated_at,next_run_at)
		 VALUES (?,?,?,?,0,?,?,?)`,
		string(id), namespace, bytes, string(recall.JobPending), now, now, now,
	); err != nil {
		return "", err
	}
	return id, nil
}

// Lease atomically picks and locks the oldest pending job whose NextRunAt has
// elapsed. Implementation uses an immediate transaction (single writer DB).
func (q *SQLiteJobQueue) Lease(ctx context.Context, now time.Time) (*recall.JobRecord, bool, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRowContext(ctx,
		`SELECT id,namespace,payload,attempts,COALESCE(last_error,''),COALESCE(entry_ids,''),created_at,updated_at,next_run_at
		   FROM memory_jobs
		  WHERE state='pending' AND next_run_at<=?
		  ORDER BY next_run_at ASC, created_at ASC LIMIT 1`,
		now.UnixMilli(),
	)
	rec, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_jobs SET state='running', attempts=attempts+1, updated_at=? WHERE id=?`,
		now.UnixMilli(), string(rec.ID),
	); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	rec.State = recall.JobRunning
	rec.Attempts++
	rec.UpdatedAt = now
	return rec, true, nil
}

// Reschedule implements recall.JobQueue.
func (q *SQLiteJobQueue) Reschedule(ctx context.Context, id recall.JobID, next time.Time, lastErr string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE memory_jobs SET state='pending', next_run_at=?, last_error=?, updated_at=? WHERE id=?`,
		next.UnixMilli(), lastErr, time.Now().UnixMilli(), string(id),
	)
	return err
}

// Complete implements recall.JobQueue.
func (q *SQLiteJobQueue) Complete(ctx context.Context, id recall.JobID, entryIDs []string) error {
	bytes, _ := json.Marshal(entryIDs)
	_, err := q.db.ExecContext(ctx,
		`UPDATE memory_jobs SET state='succeeded', entry_ids=?, updated_at=? WHERE id=?`,
		string(bytes), time.Now().UnixMilli(), string(id),
	)
	return err
}

// Fail marks the job dead.
func (q *SQLiteJobQueue) Fail(ctx context.Context, id recall.JobID, lastErr string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE memory_jobs SET state='dead', last_error=?, updated_at=? WHERE id=?`,
		lastErr, time.Now().UnixMilli(), string(id),
	)
	return err
}

// Get implements recall.JobQueue.
func (q *SQLiteJobQueue) Get(ctx context.Context, id recall.JobID) (*recall.JobRecord, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id,namespace,payload,attempts,COALESCE(last_error,''),COALESCE(entry_ids,''),created_at,updated_at,next_run_at,state
		   FROM memory_jobs WHERE id=?`,
		string(id),
	)
	rec, err := scanRecordWithState(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, recall.ErrJobNotFound
		}
		return nil, err
	}
	return rec, nil
}

// PendingCount returns count of pending jobs (used by callers / metrics).
func (q *SQLiteJobQueue) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_jobs WHERE state='pending'`).Scan(&n)
	return n, err
}

func (q *SQLiteJobQueue) newID() recall.JobID {
	q.mu.Lock()
	q.seq++
	s := q.seq
	q.mu.Unlock()
	t := uint64(time.Now().UnixMilli())
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	return recall.JobID(fmt.Sprintf("job_%013d_%016x_%08d", t, binary.BigEndian.Uint64(rnd[:]), s))
}

// -- scanners ----------------------------------------------------------------

func scanRecord(row *sql.Row) (*recall.JobRecord, error) {
	var (
		id, ns, lastErr, entryIDsJSON string
		payload                       []byte
		attempts                      int
		createdAt, updatedAt, nextRun int64
	)
	if err := row.Scan(&id, &ns, &payload, &attempts, &lastErr, &entryIDsJSON, &createdAt, &updatedAt, &nextRun); err != nil {
		return nil, err
	}
	rec := &recall.JobRecord{
		ID:        recall.JobID(id),
		Namespace: ns,
		Attempts:  attempts,
		LastError: lastErr,
		CreatedAt: time.UnixMilli(createdAt).UTC(),
		UpdatedAt: time.UnixMilli(updatedAt).UTC(),
		NextRunAt: time.UnixMilli(nextRun).UTC(),
	}
	if err := json.Unmarshal(payload, &rec.Payload); err != nil {
		return nil, err
	}
	if entryIDsJSON != "" {
		_ = json.Unmarshal([]byte(entryIDsJSON), &rec.EntryIDs)
	}
	return rec, nil
}

func scanRecordWithState(row *sql.Row) (*recall.JobRecord, error) {
	var (
		id, ns, lastErr, entryIDsJSON, state string
		payload                              []byte
		attempts                             int
		createdAt, updatedAt, nextRun        int64
	)
	if err := row.Scan(&id, &ns, &payload, &attempts, &lastErr, &entryIDsJSON, &createdAt, &updatedAt, &nextRun, &state); err != nil {
		return nil, err
	}
	rec := &recall.JobRecord{
		ID:        recall.JobID(id),
		Namespace: ns,
		Attempts:  attempts,
		LastError: lastErr,
		State:     recall.JobState(state),
		CreatedAt: time.UnixMilli(createdAt).UTC(),
		UpdatedAt: time.UnixMilli(updatedAt).UTC(),
		NextRunAt: time.UnixMilli(nextRun).UTC(),
	}
	if err := json.Unmarshal(payload, &rec.Payload); err != nil {
		return nil, err
	}
	if entryIDsJSON != "" {
		_ = json.Unmarshal([]byte(entryIDsJSON), &rec.EntryIDs)
	}
	return rec, nil
}
