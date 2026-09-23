// Package sqlite implements the memory storage contracts on a SQLite
// database. Log batches are real transactions; KV writes are single
// statements, so cross-process readers observe atomic state.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS log_events (
		stream TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		payload BLOB,
		created_at TEXT NOT NULL,
		PRIMARY KEY (stream, seq)
	)`,
	`CREATE TABLE IF NOT EXISTS log_commits (
		stream TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		first_seq INTEGER NOT NULL,
		last_seq INTEGER NOT NULL,
		digest TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (stream, idempotency_key)
	)`,
	`CREATE INDEX IF NOT EXISTS log_commits_stream_first ON log_commits (stream, first_seq)`,
	`CREATE TABLE IF NOT EXISTS kv (
		key TEXT PRIMARY KEY,
		value BLOB NOT NULL,
		updated_at TEXT NOT NULL
	)`,
}

// Store implements storage.Log, storage.CommitLog, storage.Store,
// storage.CASStore, storage.BatchStore, and storage.PutIfAbsentStore on one
// SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

var (
	_ storage.Log              = (*Store)(nil)
	_ storage.CommitLog        = (*Store)(nil)
	_ storage.Store            = (*Store)(nil)
	_ storage.CASStore         = (*Store)(nil)
	_ storage.BatchStore       = (*Store)(nil)
	_ storage.PutIfAbsentStore = (*Store)(nil)
)

// Open opens (and migrates) the SQLite database at path. The special path
// ":memory:" is allowed for tests and ephemeral runs.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("storage sqlite: path is required")
	}
	db, err := sql.Open(driverName, sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(8)
	if path == ":memory:" {
		// A shared in-memory database must keep one connection alive.
		db.SetMaxOpenConns(1)
	}
	store := &Store{db: db, path: path}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// sqliteDSN attaches the per-connection pragmas to the DSN. PRAGMAs are
// connection-scoped, so executing them once on the pool leaves every other
// pooled connection without a busy timeout; the driver applies DSN pragmas
// to every connection it opens.
func sqliteDSN(path string) string {
	if path == ":memory:" {
		// The in-memory database is pinned to one connection and migrate
		// applies the pragmas there.
		return path
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}

// Path returns the DSN this store opened.
func (store *Store) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

// Close closes the underlying database.
func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

func (store *Store) migrate(ctx context.Context) error {
	if ctx == nil {
		return errors.New("storage sqlite: context is required")
	}
	for _, statement := range schemaStatements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("storage sqlite: migrate: %w", err)
		}
	}
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := store.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("storage sqlite: pragma: %w", err)
		}
	}
	return nil
}

// Append implements storage.Log with one real transaction per batch.
func (store *Store) Append(
	ctx context.Context,
	stream string,
	events []storage.Event,
	opts storage.AppendOptions,
) (storage.Commit, error) {
	if err := validateAppend(stream, events, opts); err != nil {
		return storage.Commit{}, err
	}
	digest, err := batchDigest(stream, opts.IdempotencyKey, events)
	if err != nil {
		return storage.Commit{}, err
	}
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	existing, found, err := readCommit(ctx, conn, stream, opts.IdempotencyKey)
	if err != nil {
		return storage.Commit{}, err
	}
	if found {
		if existing.digest != digest {
			return storage.Commit{}, fmt.Errorf(
				"%w: idempotency key %q replayed with different content", storage.ErrConflict, opts.IdempotencyKey)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return storage.Commit{}, fmt.Errorf("storage sqlite: commit: %w", err)
		}
		committed = true
		return existing.commit(stream), nil
	}
	var maxSeq sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM log_events WHERE stream = ?`, stream).Scan(&maxSeq); err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: read head: %w", err)
	}
	firstSeq := uint64(1)
	if maxSeq.Valid {
		firstSeq = uint64(maxSeq.Int64) + 1
	}
	lastSeq := firstSeq + uint64(len(events)) - 1
	createdAt := time.Now().UTC()
	if opts.Metadata == nil {
		opts.Metadata = map[string]string{}
	}
	for index, event := range events {
		eventSeq := firstSeq + uint64(index)
		eventTime := event.CreatedAt
		if eventTime.IsZero() {
			eventTime = createdAt
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO log_events (stream, seq, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
			stream, int64(eventSeq), event.Type, []byte(event.Payload), eventTime.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return storage.Commit{}, fmt.Errorf("storage sqlite: insert event: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO log_commits (stream, idempotency_key, first_seq, last_seq, digest, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		stream, opts.IdempotencyKey, int64(firstSeq), int64(lastSeq), digest, createdAt.Format(time.RFC3339Nano),
	); err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: insert commit: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: commit: %w", err)
	}
	committed = true
	return storage.Commit{
		ID: commitID(stream, opts.IdempotencyKey), Stream: stream,
		FirstSeq: firstSeq, LastSeq: lastSeq,
		IdempotencyKey: opts.IdempotencyKey, CreatedAt: createdAt,
	}, nil
}

// Read implements storage.Log.
func (store *Store) Read(ctx context.Context, stream string, after uint64, limit int) ([]storage.Event, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	query := `SELECT stream, seq, type, payload, created_at FROM log_events
	          WHERE stream = ? AND seq > ? ORDER BY seq ASC`
	args := []any{stream, int64(after)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

// ReadAt implements storage.Log.
func (store *Store) ReadAt(ctx context.Context, stream string, seq uint64) (storage.Event, error) {
	if err := validateStream(stream); err != nil {
		return storage.Event{}, err
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT stream, seq, type, payload, created_at FROM log_events WHERE stream = ? AND seq = ?`,
		stream, int64(seq))
	if err != nil {
		return storage.Event{}, fmt.Errorf("storage sqlite: read at: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events, err := scanEvents(rows)
	if err != nil {
		return storage.Event{}, err
	}
	if len(events) == 0 {
		return storage.Event{}, storage.ErrNotFound
	}
	return events[0], nil
}

// ReadLatest implements storage.Log.
func (store *Store) ReadLatest(ctx context.Context, stream string, n int) ([]storage.Event, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	query := `SELECT stream, seq, type, payload, created_at FROM log_events
	          WHERE stream = ? ORDER BY seq DESC`
	args := []any{stream}
	if n > 0 {
		query += ` LIMIT ?`
		args = append(args, n)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: read latest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

// ListStreams implements storage.Log.
func (store *Store) ListStreams(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := validateName(prefix); err != nil {
			return nil, err
		}
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT DISTINCT stream FROM log_events
		 WHERE stream = ? OR stream LIKE ? ESCAPE '\' ORDER BY stream ASC`,
		prefix, likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: list streams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	streams := make([]string, 0)
	for rows.Next() {
		var stream string
		if err := rows.Scan(&stream); err != nil {
			return nil, err
		}
		if prefix == "" || stream == prefix || strings.HasPrefix(stream, prefix+"/") {
			streams = append(streams, stream)
		}
	}
	return streams, rows.Err()
}

// ListCommits implements storage.CommitLog.
func (store *Store) ListCommits(ctx context.Context, stream string, after uint64, limit int) ([]storage.Commit, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	query := `SELECT idempotency_key, first_seq, last_seq, digest, created_at FROM log_commits
	          WHERE stream = ? AND first_seq > ? ORDER BY first_seq ASC`
	args := []any{stream, int64(after)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: list commits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	commits := make([]storage.Commit, 0)
	for rows.Next() {
		commit, err := scanCommit(rows, stream)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	return commits, rows.Err()
}

// ReadCommitByKey implements storage.CommitLog.
func (store *Store) ReadCommitByKey(ctx context.Context, stream, idempotencyKey string) (storage.Commit, bool, error) {
	if err := validateStream(stream); err != nil {
		return storage.Commit{}, false, err
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return storage.Commit{}, false, errors.New("storage sqlite: idempotency key is required")
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT idempotency_key, first_seq, last_seq, digest, created_at FROM log_commits
		 WHERE stream = ? AND idempotency_key = ?`,
		stream, idempotencyKey)
	if err != nil {
		return storage.Commit{}, false, fmt.Errorf("storage sqlite: read commit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return storage.Commit{}, false, rows.Err()
	}
	commit, err := scanCommit(rows, stream)
	if err != nil {
		return storage.Commit{}, false, err
	}
	return commit, true, rows.Err()
}

// Get implements storage.Store.
func (store *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validateName(key); err != nil {
		return nil, err
	}
	var value []byte
	err := store.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: kv get: %w", err)
	}
	return value, nil
}

// Put implements storage.Store.
func (store *Store) Put(ctx context.Context, key string, data []byte) error {
	if err := validateName(key); err != nil {
		return err
	}
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, data, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("storage sqlite: kv put: %w", err)
	}
	return nil
}

// Delete implements storage.Store; deleting a missing key is a no-op.
func (store *Store) Delete(ctx context.Context, key string) error {
	if err := validateName(key); err != nil {
		return err
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key); err != nil {
		return fmt.Errorf("storage sqlite: kv delete: %w", err)
	}
	return nil
}

// List implements storage.Store.
func (store *Store) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	if prefix != "" {
		if err := validateName(prefix); err != nil {
			return nil, err
		}
	}
	query := `SELECT key, value FROM kv ORDER BY key ASC`
	var arguments []any
	if prefix != "" {
		query = `SELECT key, value FROM kv WHERE key = ? OR key LIKE ? ESCAPE '\' ORDER BY key ASC`
		arguments = []any{prefix, likePrefix(prefix)}
	}
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("storage sqlite: kv list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]storage.Entry, 0)
	for rows.Next() {
		var entry storage.Entry
		if err := rows.Scan(&entry.Key, &entry.Value); err != nil {
			return nil, err
		}
		if prefix == "" || entry.Key == prefix || strings.HasPrefix(entry.Key, prefix+"/") {
			entries = append(entries, entry)
		}
	}
	return entries, rows.Err()
}

// PutIfAbsent implements storage.PutIfAbsentStore.
func (store *Store) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	if err := validateName(key); err != nil {
		return false, err
	}
	result, err := store.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO NOTHING`,
		key, data, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("storage sqlite: kv put if absent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// CompareAndSwap implements storage.CASStore. A missing key never matches.
func (store *Store) CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error) {
	if err := validateName(key); err != nil {
		return false, err
	}
	result, err := store.db.ExecContext(ctx,
		`UPDATE kv SET value = ?, updated_at = ? WHERE key = ? AND value = ?`,
		new, time.Now().UTC().Format(time.RFC3339Nano), key, old)
	if err != nil {
		return false, fmt.Errorf("storage sqlite: kv compare and swap: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// PutBatch implements storage.BatchStore with one transaction per batch.
func (store *Store) PutBatch(ctx context.Context, entries []storage.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage sqlite: kv batch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, entry := range entries {
		if err := validateName(entry.Key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			entry.Key, entry.Value, now,
		); err != nil {
			return fmt.Errorf("storage sqlite: kv batch put: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage sqlite: kv batch commit: %w", err)
	}
	return nil
}

type persistedCommit struct {
	key       string
	firstSeq  uint64
	lastSeq   uint64
	digest    string
	createdAt time.Time
}

func (commit persistedCommit) commit(stream string) storage.Commit {
	return storage.Commit{
		ID: commitID(stream, commit.key), Stream: stream,
		FirstSeq: commit.firstSeq, LastSeq: commit.lastSeq,
		IdempotencyKey: commit.key, CreatedAt: commit.createdAt,
	}
}

func scanCommit(rows *sql.Rows, stream string) (storage.Commit, error) {
	var key, digest, createdAt string
	var firstSeq, lastSeq int64
	if err := rows.Scan(&key, &firstSeq, &lastSeq, &digest, &createdAt); err != nil {
		return storage.Commit{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return storage.Commit{}, fmt.Errorf("storage sqlite: parse commit time: %w", err)
	}
	return persistedCommit{
		key: key, firstSeq: uint64(firstSeq), lastSeq: uint64(lastSeq),
		digest: digest, createdAt: parsed,
	}.commit(stream), nil
}

func scanEvents(rows *sql.Rows) ([]storage.Event, error) {
	events := make([]storage.Event, 0)
	for rows.Next() {
		var (
			stream, eventType, createdAt string
			seq                          int64
			payload                      []byte
		)
		if err := rows.Scan(&stream, &seq, &eventType, &payload, &createdAt); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("storage sqlite: parse event time: %w", err)
		}
		events = append(events, storage.Event{
			Stream: stream, Seq: uint64(seq), Type: eventType,
			Payload: json.RawMessage(payload), CreatedAt: parsed,
		})
	}
	return events, rows.Err()
}

func readCommit(ctx context.Context, conn *sql.Conn, stream, key string) (persistedCommit, bool, error) {
	var digest, createdAt string
	var firstSeq, lastSeq int64
	err := conn.QueryRowContext(ctx,
		`SELECT first_seq, last_seq, digest, created_at FROM log_commits WHERE stream = ? AND idempotency_key = ?`,
		stream, key).Scan(&firstSeq, &lastSeq, &digest, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return persistedCommit{}, false, nil
	}
	if err != nil {
		return persistedCommit{}, false, fmt.Errorf("storage sqlite: read commit: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return persistedCommit{}, false, fmt.Errorf("storage sqlite: parse commit time: %w", err)
	}
	return persistedCommit{
		key: key, firstSeq: uint64(firstSeq), lastSeq: uint64(lastSeq),
		digest: digest, createdAt: parsed,
	}, true, nil
}

func validateAppend(stream string, events []storage.Event, opts storage.AppendOptions) error {
	if err := validateStream(stream); err != nil {
		return err
	}
	if strings.TrimSpace(opts.IdempotencyKey) == "" {
		return errors.New("storage sqlite: idempotency key is required")
	}
	if len(events) == 0 {
		return errors.New("storage sqlite: events are required")
	}
	for index, event := range events {
		if event.Stream != stream {
			return fmt.Errorf("storage sqlite: event %d stream %q does not match %q", index, event.Stream, stream)
		}
		if strings.TrimSpace(event.Type) == "" {
			return fmt.Errorf("storage sqlite: event %d type is required", index)
		}
	}
	return nil
}

func validateStream(stream string) error {
	return validateName(stream)
}

func validateName(name string) error {
	if name == "" {
		return errors.New("storage sqlite: name is required")
	}
	if strings.ContainsRune(name, '\x00') {
		return errors.New("storage sqlite: name must not contain NUL")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" {
			return errors.New("storage sqlite: name has an empty segment")
		}
		if segment == "." || segment == ".." {
			return errors.New("storage sqlite: name must not contain dot segments")
		}
	}
	return nil
}

// likePrefix escapes LIKE metacharacters and appends the segment wildcard.
func likePrefix(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "/%"
}

type digestEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func batchDigest(stream, key string, events []storage.Event) (string, error) {
	wire := make([]digestEvent, len(events))
	for index, event := range events {
		wire[index] = digestEvent{Type: event.Type, Payload: event.Payload}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(stream+"\x00"+key+"\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func commitID(stream, key string) string {
	sum := sha256.Sum256([]byte(stream + "\x00" + key))
	return "commit-" + hex.EncodeToString(sum[:])
}
