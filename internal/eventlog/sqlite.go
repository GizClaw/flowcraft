package eventlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/internal/store"
)

// SQLiteLog is the production Log implementation backed by SQLite, fronted by
// a bounded ring buffer for fast in-process subscribers.
//
// All writes go through Atomic(). Subscribers register via Subscribe() and are
// fanned out from the post-commit hook on the same goroutine; if a subscriber
// channel is full and dropPolicy=drop, the event is dropped and Lag() reports
// the gap. dropPolicy=block back-pressures the writer and is reserved for
// internal projectors that must not lose events.
type SQLiteLog struct {
	db  *sql.DB
	rng *ring

	// writeMu serialises Atomic so the next-seq prediction in subscribers
	// (and the ring buffer) sees a single, monotonic stream of commits.
	// SQLite already serialises writers, but we also need to serialise the
	// post-commit fan-out so subscribers never observe seq out-of-order.
	writeMu sync.Mutex

	// subscribers is the live fan-out registry, guarded by mu.
	mu          sync.RWMutex
	subscribers []*subscription

	closed atomic.Bool
}

const defaultRingCapacity = 4096

// NewSQLiteLog creates a SQLite-backed event log using the shared store DB.
// The store must have run migrations 006+ first.
func NewSQLiteLog(db *sql.DB) *SQLiteLog {
	return &SQLiteLog{db: db, rng: newRing(defaultRingCapacity)}
}

// NewSQLiteLogFromStore is a convenience that extracts the DB from a SQLiteStore.
func NewSQLiteLogFromStore(s *store.SQLiteStore) *SQLiteLog {
	return NewSQLiteLog(s.DB())
}

var _ Log = (*SQLiteLog)(nil)

// DB returns the underlying *sql.DB. Used by bridges/projectors that need to
// share the same connection (and therefore the same WAL writer).
func (l *SQLiteLog) DB() *sql.DB { return l.db }

// Atomic executes fn inside a single SQLite write transaction. Either every
// envelope appended through uow.Append() commits, or none of them do. The
// returned slice contains the envelopes with their final, on-disk seq.
//
// Subscribers are notified after the commit succeeds, in order, so they never
// observe an envelope whose seq is not yet visible to a fresh Read().
func (l *SQLiteLog) Atomic(ctx context.Context, fn func(uow UnitOfWork) error) ([]Envelope, error) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("eventlog: begin tx: %w", err)
	}

	uow := &sqliteUow{tx: tx, ctx: ctx}
	if err := fn(uow); err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("eventlog: commit: %w", err)
	}

	envs := uow.appended
	if len(envs) > 0 {
		l.rng.appendBulk(envs)
		l.fanout(envs)
	}
	return envs, nil
}

// appendOne is the single-envelope write path used exclusively by the
// generated PublishXxx helpers. It is unexported so business packages
// cannot bypass the publisher API and append raw envelopes (§11.1#5).
// Callers wanting to bundle multiple events must use Atomic.
func (l *SQLiteLog) appendOne(ctx context.Context, env Envelope) (int64, error) {
	envs, err := l.Atomic(ctx, func(uow UnitOfWork) error {
		return uow.Append(ctx, EnvelopeDraft{
			Partition: env.Partition,
			Type:      env.Type,
			Version:   env.Version,
			Category:  env.Category,
			Payload:   json.RawMessage(env.Payload),
			TraceID:   env.TraceID,
			SpanID:    env.SpanID,
			Actor:     env.Actor,
		})
	})
	if err != nil {
		return 0, err
	}
	if len(envs) == 0 {
		return 0, nil
	}
	return envs[len(envs)-1].Seq, nil
}

// LatestSeq returns the highest committed seq in the log, or 0 if empty.
func (l *SQLiteLog) LatestSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	row := l.db.QueryRowContext(ctx, "SELECT MAX(seq) FROM event_log")
	if err := row.Scan(&seq); err != nil {
		return 0, fmt.Errorf("eventlog: latest seq: %w", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

// LatestInPartition returns the (seq, ts) of the newest envelope in the
// given partition. Returns (0, zero, nil) when the partition is empty so
// snapshot endpoints can return a usable cursor for live tailing
// (`since=0` meaning "give me everything from the start").
func (l *SQLiteLog) LatestInPartition(ctx context.Context, partition string) (int64, time.Time, error) {
	var (
		seq sql.NullInt64
		ts  sql.NullString
	)
	row := l.db.QueryRowContext(ctx,
		`SELECT seq, ts FROM event_log
		 WHERE partition=? ORDER BY seq DESC LIMIT 1`, partition)
	if err := row.Scan(&seq, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, fmt.Errorf("eventlog: latest in partition: %w", err)
	}
	if !seq.Valid {
		return 0, time.Time{}, nil
	}
	if !ts.Valid || ts.String == "" {
		return seq.Int64, time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts.String)
	if err != nil {
		return seq.Int64, time.Time{}, nil
	}
	return seq.Int64, parsed, nil
}

// Checkpoints returns the projector checkpoint sub-store (SQLite-backed).
func (l *SQLiteLog) Checkpoints() CheckpointStore { return &sqliteCheckpointStore{db: l.db} }

// Read returns events for a single partition with seq > since, up to limit.
// since == SinceLive returns the empty page so HTTP pull clients can tail
// without re-reading history (live streaming should use Subscribe instead).
func (l *SQLiteLog) Read(ctx context.Context, partition string, since Since, limit int) (ReadResult, error) {
	if limit <= 0 {
		limit = 200
	}
	if since == SinceLive {
		max, err := l.LatestSeq(ctx)
		if err != nil {
			return ReadResult{}, err
		}
		return ReadResult{NextSince: Since(max)}, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT seq,partition,type,version,category,ts,payload,trace_id,span_id,actor_id,actor_kind,actor_realm_id
		FROM event_log WHERE partition=? AND seq>? ORDER BY seq ASC LIMIT ?`,
		partition, int64(since), limit)
	if err != nil {
		return ReadResult{}, fmt.Errorf("eventlog: read: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows, limit, int64(since))
}

// ReadAll is identical to Read but without the partition filter; reserved for
// admin/replay tools.
func (l *SQLiteLog) ReadAll(ctx context.Context, since Since, limit int) (ReadResult, error) {
	if limit <= 0 {
		limit = 200
	}
	if since == SinceLive {
		max, err := l.LatestSeq(ctx)
		if err != nil {
			return ReadResult{}, err
		}
		return ReadResult{NextSince: Since(max)}, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT seq,partition,type,version,category,ts,payload,trace_id,span_id,actor_id,actor_kind,actor_realm_id
		FROM event_log WHERE seq>? ORDER BY seq ASC LIMIT ?`,
		int64(since), limit)
	if err != nil {
		return ReadResult{}, fmt.Errorf("eventlog: read all: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows, limit, int64(since))
}

// fanout pushes envs to every live subscriber. It runs while writeMu is still
// held by the committer, so subscribers see a strict total order.
func (l *SQLiteLog) fanout(envs []Envelope) {
	l.mu.RLock()
	subs := make([]*subscription, len(l.subscribers))
	copy(subs, l.subscribers)
	l.mu.RUnlock()
	for _, s := range subs {
		s.deliver(envs)
	}
}

// removeSubscriber drops s from the registry; called from subscription.Close.
func (l *SQLiteLog) removeSubscriber(s *subscription) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, x := range l.subscribers {
		if x == s {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
			return
		}
	}
}

// addSubscriber registers s with the live fan-out.
func (l *SQLiteLog) addSubscriber(s *subscription) {
	l.mu.Lock()
	l.subscribers = append(l.subscribers, s)
	l.mu.Unlock()
}

// ---- sqliteCheckpointStore ----

type sqliteCheckpointStore struct{ db *sql.DB }

func (s *sqliteCheckpointStore) Get(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		"SELECT checkpoint_seq FROM projector_checkpoints WHERE projector_name=?", name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("eventlog: get checkpoint: %w", err)
	}
	return seq, nil
}

func (s *sqliteCheckpointStore) List(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT projector_name,checkpoint_seq FROM projector_checkpoints")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, rows.Err()
}

func (s *sqliteCheckpointStore) Min(ctx context.Context) (int64, bool, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MIN(checkpoint_seq) FROM projector_checkpoints").Scan(&v); err != nil {
		return 0, false, err
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// ---- sqliteUow ----

type sqliteUow struct {
	tx       *sql.Tx
	ctx      context.Context
	appended []Envelope
	lastSeq  int64
}

func (u *sqliteUow) BusinessExec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return u.tx.ExecContext(ctx, q, args...)
}
func (u *sqliteUow) BusinessQuery(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return u.tx.QueryContext(ctx, q, args...)
}
func (u *sqliteUow) BusinessQueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return u.tx.QueryRowContext(ctx, q, args...)
}

func (u *sqliteUow) Append(ctx context.Context, drafts ...EnvelopeDraft) error {
	for _, d := range drafts {
		if d.Partition == "" || d.Type == "" || d.Category == "" || d.Version == 0 {
			return fmt.Errorf("eventlog: incomplete draft (partition/type/category/version required): %+v", d)
		}
		payload, err := marshalPayload(d.Payload)
		if err != nil {
			return fmt.Errorf("eventlog: marshal payload: %w", err)
		}
		var actorID, actorKind, actorRealm sql.NullString
		if d.Actor != nil {
			if d.Actor.ID != "" {
				actorID = sql.NullString{String: d.Actor.ID, Valid: true}
			}
			if d.Actor.Kind != "" {
				actorKind = sql.NullString{String: d.Actor.Kind, Valid: true}
			}
			if d.Actor.RealmID != "" {
				actorRealm = sql.NullString{String: d.Actor.RealmID, Valid: true}
			}
		}
		ts := Time()
		res, err := u.tx.ExecContext(ctx, `
			INSERT INTO event_log(partition,type,version,category,ts,payload,trace_id,span_id,actor_id,actor_kind,actor_realm_id)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			d.Partition, d.Type, d.Version, d.Category, ts, payload,
			nullString(d.TraceID), nullString(d.SpanID), actorID, actorKind, actorRealm)
		if err != nil {
			return fmt.Errorf("eventlog: insert: %w", err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("eventlog: last insert id: %w", err)
		}
		u.lastSeq = seq
		env := Envelope{
			Seq:       seq,
			Partition: d.Partition,
			Type:      d.Type,
			Version:   d.Version,
			Category:  d.Category,
			Ts:        ts,
			Payload:   payload,
			TraceID:   d.TraceID,
			SpanID:    d.SpanID,
		}
		if d.Actor != nil {
			a := *d.Actor
			env.Actor = &a
		}
		u.appended = append(u.appended, env)
	}
	return nil
}

func (u *sqliteUow) CheckpointSet(ctx context.Context, name string, seq int64) error {
	ts := Time()
	_, err := u.tx.ExecContext(ctx, `
		INSERT INTO projector_checkpoints(projector_name,checkpoint_seq,updated_at)
		VALUES(?,?,?)
		ON CONFLICT(projector_name) DO UPDATE SET
		  checkpoint_seq=excluded.checkpoint_seq,
		  updated_at=excluded.updated_at
		WHERE excluded.checkpoint_seq>=projector_checkpoints.checkpoint_seq`,
		name, seq, ts)
	if err != nil {
		return fmt.Errorf("eventlog: checkpoint set: %w", err)
	}
	return nil
}

func (u *sqliteUow) Sequence() int64 { return u.lastSeq }

// ---- helpers ----

func marshalPayload(p any) (json.RawMessage, error) {
	if p == nil {
		return json.RawMessage("null"), nil
	}
	if raw, ok := p.(json.RawMessage); ok {
		return raw, nil
	}
	if b, ok := p.([]byte); ok {
		return json.RawMessage(b), nil
	}
	return json.Marshal(p)
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func scanEvents(rows *sql.Rows, limit int, since int64) (ReadResult, error) {
	envs := make([]Envelope, 0, limit)
	for rows.Next() {
		var env Envelope
		var traceID, spanID, actorID, actorKind, actorRealm sql.NullString
		var payload []byte
		if err := rows.Scan(&env.Seq, &env.Partition, &env.Type, &env.Version,
			&env.Category, &env.Ts, &payload, &traceID, &spanID,
			&actorID, &actorKind, &actorRealm); err != nil {
			return ReadResult{}, err
		}
		env.Payload = payload
		env.TraceID = traceID.String
		env.SpanID = spanID.String
		if actorID.Valid || actorKind.Valid || actorRealm.Valid {
			env.Actor = &Actor{
				ID:      actorID.String,
				Kind:    actorKind.String,
				RealmID: actorRealm.String,
			}
		}
		envs = append(envs, env)
	}
	if err := rows.Err(); err != nil {
		return ReadResult{}, err
	}
	hasMore := len(envs) == limit
	next := Since(since)
	if len(envs) > 0 {
		next = Since(envs[len(envs)-1].Seq)
	}
	return ReadResult{Events: envs, NextSince: next, HasMore: hasMore}, nil
}
