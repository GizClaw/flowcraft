// Package postgres implements the memory storage contracts on PostgreSQL.
// Log batches are real transactions serialized per stream with an advisory
// lock, so concurrent appenders cannot interleave sequence assignment.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultSchema = "flowcraft_memory"

var schemaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// NormalizeSchema resolves the schema a config asks for into the one the store
// will use: an omitted schema means DefaultSchema.
func NormalizeSchema(schema string) string {
	if schema == "" {
		return DefaultSchema
	}
	return schema
}

// PoolKey identifies one PostgreSQL connection pool: the DSN plus the
// normalized schema. Callers share a pool by comparing this against
// Store.DSNKey, which is why the schema is normalized here rather than at the
// call site -- a config that omits "schema" and one that names DefaultSchema
// describe the same pool and must compare equal.
func PoolKey(dsn, schema string) string {
	return dsn + "\x00" + NormalizeSchema(schema)
}

// Store implements the storage contracts on one PostgreSQL schema.
type Store struct {
	pool   *pgxpool.Pool
	schema string
	key    string
}

var (
	_ storage.Log              = (*Store)(nil)
	_ storage.CommitLog        = (*Store)(nil)
	_ storage.Store            = (*Store)(nil)
	_ storage.CASStore         = (*Store)(nil)
	_ storage.BatchStore       = (*Store)(nil)
	_ storage.PutIfAbsentStore = (*Store)(nil)
)

// Open connects to dsn and migrates the target schema.
func Open(ctx context.Context, dsn, schema string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("storage postgres: dsn is required")
	}
	schema = NormalizeSchema(schema)
	if !schemaPattern.MatchString(schema) {
		return nil, fmt.Errorf("storage postgres: invalid schema %q", schema)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage postgres: ping: %w", err)
	}
	store := &Store{pool: pool, schema: schema, key: PoolKey(dsn, schema)}
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the connection pool.
func (store *Store) Close() error {
	if store == nil || store.pool == nil {
		return nil
	}
	store.pool.Close()
	return nil
}

// Schema returns the schema this store manages.
func (store *Store) Schema() string {
	if store == nil {
		return ""
	}
	return store.schema
}

// DSNKey identifies one connection + schema pair for driver de-duplication.
func (store *Store) DSNKey() string {
	if store == nil {
		return ""
	}
	return store.key
}

// Reset truncates every table; it exists for tests and operational repair.
func (store *Store) Reset(ctx context.Context) error {
	for _, table := range []string{"log_events", "log_commits", "kv"} {
		if _, err := store.pool.Exec(ctx, "TRUNCATE TABLE "+store.table(table)); err != nil {
			return fmt.Errorf("storage postgres: reset %s: %w", table, err)
		}
	}
	return nil
}

func (store *Store) table(name string) string {
	return pgx.Identifier{store.schema, name}.Sanitize()
}

func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + pgx.Identifier{store.schema}.Sanitize(),
		`CREATE TABLE IF NOT EXISTS ` + store.table("log_events") + ` (
			stream text NOT NULL,
			seq bigint NOT NULL,
			type text NOT NULL,
			payload bytea,
			created_at timestamptz NOT NULL,
			PRIMARY KEY (stream, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + store.table("log_commits") + ` (
			stream text NOT NULL,
			idempotency_key text NOT NULL,
			first_seq bigint NOT NULL,
			last_seq bigint NOT NULL,
			digest text NOT NULL,
			created_at timestamptz NOT NULL,
			PRIMARY KEY (stream, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS log_commits_stream_first ON ` + store.table("log_commits") + ` (stream, first_seq)`,
		`CREATE TABLE IF NOT EXISTS ` + store.table("kv") + ` (
			key text PRIMARY KEY,
			value bytea NOT NULL,
			updated_at timestamptz NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := store.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("storage postgres: migrate: %w", err)
		}
	}
	return nil
}

// Append implements storage.Log.
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
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return storage.Commit{}, fmt.Errorf("storage postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, stream); err != nil {
		return storage.Commit{}, fmt.Errorf("storage postgres: lock stream: %w", err)
	}
	existing, found, err := readCommit(ctx, tx, store.table("log_commits"), stream, opts.IdempotencyKey)
	if err != nil {
		return storage.Commit{}, err
	}
	if found {
		if existing.digest != digest {
			return storage.Commit{}, fmt.Errorf(
				"%w: idempotency key %q replayed with different content", storage.ErrConflict, opts.IdempotencyKey)
		}
		if err := tx.Commit(ctx); err != nil {
			return storage.Commit{}, fmt.Errorf("storage postgres: commit: %w", err)
		}
		return existing.commit(stream), nil
	}
	var maxSeq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM `+store.table("log_events")+` WHERE stream = $1`, stream).Scan(&maxSeq); err != nil {
		return storage.Commit{}, fmt.Errorf("storage postgres: read head: %w", err)
	}
	firstSeq := uint64(maxSeq) + 1
	lastSeq := firstSeq + uint64(len(events)) - 1
	createdAt := time.Now().UTC()
	for index, event := range events {
		eventTime := event.CreatedAt
		if eventTime.IsZero() {
			eventTime = createdAt
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO `+store.table("log_events")+` (stream, seq, type, payload, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			stream, int64(firstSeq)+int64(index), event.Type, []byte(event.Payload), eventTime.UTC(),
		); err != nil {
			return storage.Commit{}, fmt.Errorf("storage postgres: insert event: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+store.table("log_commits")+` (stream, idempotency_key, first_seq, last_seq, digest, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		stream, opts.IdempotencyKey, int64(firstSeq), int64(lastSeq), digest, createdAt,
	); err != nil {
		return storage.Commit{}, fmt.Errorf("storage postgres: insert commit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.Commit{}, fmt.Errorf("storage postgres: commit: %w", err)
	}
	return storage.Commit{
		ID: commitID(stream, opts.IdempotencyKey), Stream: stream,
		FirstSeq: firstSeq, LastSeq: lastSeq, IdempotencyKey: opts.IdempotencyKey, CreatedAt: createdAt,
	}, nil
}

// Read implements storage.Log.
func (store *Store) Read(ctx context.Context, stream string, after uint64, limit int) ([]storage.Event, error) {
	if err := validateStream(stream); err != nil {
		return nil, err
	}
	query := `SELECT stream, seq, type, payload, created_at FROM ` + store.table("log_events") +
		` WHERE stream = $1 AND seq > $2 ORDER BY seq ASC`
	args := []any{stream, int64(after)}
	if limit > 0 {
		query += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: read: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ReadAt implements storage.Log.
func (store *Store) ReadAt(ctx context.Context, stream string, seq uint64) (storage.Event, error) {
	if err := validateStream(stream); err != nil {
		return storage.Event{}, err
	}
	rows, err := store.pool.Query(ctx,
		`SELECT stream, seq, type, payload, created_at FROM `+store.table("log_events")+
			` WHERE stream = $1 AND seq = $2`, stream, int64(seq))
	if err != nil {
		return storage.Event{}, fmt.Errorf("storage postgres: read at: %w", err)
	}
	defer rows.Close()
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
	query := `SELECT stream, seq, type, payload, created_at FROM ` + store.table("log_events") +
		` WHERE stream = $1 ORDER BY seq DESC`
	args := []any{stream}
	if n > 0 {
		query += ` LIMIT $2`
		args = append(args, n)
	}
	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: read latest: %w", err)
	}
	defer rows.Close()
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
	// An empty prefix means "every stream" (the workspace driver reads it that
	// way, and the post-filter below keeps that meaning): the LIKE arm cannot
	// express it, since "/%" matches no stream name.
	query := `SELECT DISTINCT stream FROM ` + store.table("log_events") +
		` WHERE stream = $1 OR stream LIKE $2 ESCAPE '\' ORDER BY stream ASC`
	arguments := []any{prefix, likePrefix(prefix)}
	if prefix == "" {
		query = `SELECT DISTINCT stream FROM ` + store.table("log_events") + ` ORDER BY stream ASC`
		arguments = nil
	}
	rows, err := store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: list streams: %w", err)
	}
	defer rows.Close()
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
	query := `SELECT idempotency_key, first_seq, last_seq, digest, created_at FROM ` + store.table("log_commits") +
		` WHERE stream = $1 AND first_seq > $2 ORDER BY first_seq ASC`
	args := []any{stream, int64(after)}
	if limit > 0 {
		query += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := store.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: list commits: %w", err)
	}
	defer rows.Close()
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
		return storage.Commit{}, false, errors.New("storage postgres: idempotency key is required")
	}
	rows, err := store.pool.Query(ctx,
		`SELECT idempotency_key, first_seq, last_seq, digest, created_at FROM `+store.table("log_commits")+
			` WHERE stream = $1 AND idempotency_key = $2`, stream, idempotencyKey)
	if err != nil {
		return storage.Commit{}, false, fmt.Errorf("storage postgres: read commit: %w", err)
	}
	defer rows.Close()
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
	err := store.pool.QueryRow(ctx, `SELECT value FROM `+store.table("kv")+` WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage postgres: kv get: %w", err)
	}
	return value, nil
}

// Put implements storage.Store.
func (store *Store) Put(ctx context.Context, key string, data []byte) error {
	if err := validateName(key); err != nil {
		return err
	}
	_, err := store.pool.Exec(ctx,
		`INSERT INTO `+store.table("kv")+` (key, value, updated_at) VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, data, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("storage postgres: kv put: %w", err)
	}
	return nil
}

// Delete implements storage.Store; deleting a missing key is a no-op.
func (store *Store) Delete(ctx context.Context, key string) error {
	if err := validateName(key); err != nil {
		return err
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM `+store.table("kv")+` WHERE key = $1`, key); err != nil {
		return fmt.Errorf("storage postgres: kv delete: %w", err)
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
	query := `SELECT key, value FROM ` + store.table("kv") + ` ORDER BY key ASC`
	var arguments []any
	if prefix != "" {
		query = `SELECT key, value FROM ` + store.table("kv") +
			` WHERE key = $1 OR key LIKE $2 ESCAPE '\' ORDER BY key ASC`
		arguments = []any{prefix, likePrefix(prefix)}
	}
	rows, err := store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("storage postgres: kv list: %w", err)
	}
	defer rows.Close()
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
	result, err := store.pool.Exec(ctx,
		`INSERT INTO `+store.table("kv")+` (key, value, updated_at) VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING`,
		key, data, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("storage postgres: kv put if absent: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

// CompareAndSwap implements storage.CASStore. A missing key never matches.
func (store *Store) CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error) {
	if err := validateName(key); err != nil {
		return false, err
	}
	result, err := store.pool.Exec(ctx,
		`UPDATE `+store.table("kv")+` SET value = $1, updated_at = $2 WHERE key = $3 AND value = $4`,
		new, time.Now().UTC(), key, old)
	if err != nil {
		return false, fmt.Errorf("storage postgres: kv compare and swap: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

// PutBatch implements storage.BatchStore with one transaction per batch.
func (store *Store) PutBatch(ctx context.Context, entries []storage.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage postgres: kv batch begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UTC()
	for _, entry := range entries {
		if err := validateName(entry.Key); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO `+store.table("kv")+` (key, value, updated_at) VALUES ($1, $2, $3)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			entry.Key, entry.Value, now,
		); err != nil {
			return fmt.Errorf("storage postgres: kv batch put: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage postgres: kv batch commit: %w", err)
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

type pgxRows interface {
	Scan(dest ...any) error
	Next() bool
	Err() error
	Close()
}

func scanEvents(rows pgxRows) ([]storage.Event, error) {
	events := make([]storage.Event, 0)
	for rows.Next() {
		var (
			stream, eventType string
			seq               int64
			payload           []byte
			createdAt         time.Time
		)
		if err := rows.Scan(&stream, &seq, &eventType, &payload, &createdAt); err != nil {
			return nil, err
		}
		events = append(events, storage.Event{
			Stream: stream, Seq: uint64(seq), Type: eventType,
			Payload: json.RawMessage(payload), CreatedAt: createdAt.UTC(),
		})
	}
	return events, rows.Err()
}

func scanCommit(rows pgxRows, stream string) (storage.Commit, error) {
	var key, digest string
	var firstSeq, lastSeq int64
	var createdAt time.Time
	if err := rows.Scan(&key, &firstSeq, &lastSeq, &digest, &createdAt); err != nil {
		return storage.Commit{}, err
	}
	return persistedCommit{
		key: key, firstSeq: uint64(firstSeq), lastSeq: uint64(lastSeq),
		digest: digest, createdAt: createdAt.UTC(),
	}.commit(stream), nil
}

func readCommit(ctx context.Context, tx pgx.Tx, table, stream, key string) (persistedCommit, bool, error) {
	var digest string
	var firstSeq, lastSeq int64
	var createdAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT first_seq, last_seq, digest, created_at FROM `+table+` WHERE stream = $1 AND idempotency_key = $2`,
		stream, key).Scan(&firstSeq, &lastSeq, &digest, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return persistedCommit{}, false, nil
	}
	if err != nil {
		return persistedCommit{}, false, fmt.Errorf("storage postgres: read commit: %w", err)
	}
	return persistedCommit{
		key: key, firstSeq: uint64(firstSeq), lastSeq: uint64(lastSeq),
		digest: digest, createdAt: createdAt.UTC(),
	}, true, nil
}

func validateAppend(stream string, events []storage.Event, opts storage.AppendOptions) error {
	if err := validateStream(stream); err != nil {
		return err
	}
	if strings.TrimSpace(opts.IdempotencyKey) == "" {
		return errors.New("storage postgres: idempotency key is required")
	}
	if len(events) == 0 {
		return errors.New("storage postgres: events are required")
	}
	for index, event := range events {
		if event.Stream != stream {
			return fmt.Errorf("storage postgres: event %d stream %q does not match %q", index, event.Stream, stream)
		}
		if strings.TrimSpace(event.Type) == "" {
			return fmt.Errorf("storage postgres: event %d type is required", index)
		}
	}
	return nil
}

func validateStream(stream string) error {
	return validateName(stream)
}

func validateName(name string) error {
	if name == "" {
		return errors.New("storage postgres: name is required")
	}
	if strings.ContainsRune(name, '\x00') {
		return errors.New("storage postgres: name must not contain NUL")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" {
			return errors.New("storage postgres: name has an empty segment")
		}
		if segment == "." || segment == ".." {
			return errors.New("storage postgres: name must not contain dot segments")
		}
	}
	return nil
}

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
