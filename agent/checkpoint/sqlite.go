// Package checkpoint stores agent tasks on the local filesystem with SQLite.
//
// The schema is two tables plus a blob table, and it is written to translate:
//
//	tasks   one row per Task: the immutable seed, the mutable projection, a version,
//	        and a save counter that orders a session's tasks
//	events  the appended facts, one row per fact, keyed (task_id, seq)
//	blobs   artifact bytes by content hash
//
// "The newest task of a session" — the one a later turn carries from — is the
// greatest save counter, never a timestamp: see savedAtLayout for what the text
// comparison got wrong.
//
// Every save is one transaction: append the round's events and update the
// projection together, so a reader never sees a task that mentions a message the
// event log does not have. A round is the unit of durability; a partial one is
// never visible.
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
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// emptyPayload is what a fact without one is stored as: the column is NOT NULL, and a caller that
// never set a payload still recorded a fact.
var emptyPayload = json.RawMessage(`null`)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	task_id    TEXT PRIMARY KEY,
	context_id TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL,
	round      INTEGER NOT NULL DEFAULT 0,
	schema     INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	seed           BLOB NOT NULL DEFAULT '',
	body           BLOB NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1,
	server_version INTEGER NOT NULL DEFAULT 0,
	saved_at       INTEGER NOT NULL DEFAULT 0,
	updated_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	task_id TEXT    NOT NULL,
	seq     INTEGER NOT NULL,
	kind    TEXT    NOT NULL,
	payload BLOB    NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (task_id, seq)
);
CREATE TABLE IF NOT EXISTS blobs (
	ref  TEXT PRIMARY KEY,
	body BLOB NOT NULL
);`

// savedAtLayout is how updated_at is written: a fixed nine-digit fraction, so
// the text sorts the way the clock does.
//
// time.RFC3339Nano trims trailing zeros, which makes the column's order depend on
// how the clock landed rather than on which row is newer: "…00.1234Z" compares
// greater than "…00.123456Z" because 'Z' sorts after '4', yet the second value is
// 56 microseconds later. "The newest task of this session" is read from that
// order, so a session could inherit the task before its previous one — one turn's
// conversation lost from the carry. Ordering uses saved_at (below) for the same
// reason: an integer cannot mis-sort.
const savedAtLayout = "2006-01-02T15:04:05.000000000Z07:00"

// SQLite is the database/sql backend. A Postgres backend is a sibling file
// with the same shape, which is why nothing here is named "local".
// SQLite satisfies the narrow contract a driver is handed as well as the full one: the host
// passes its store through agent.DriverStoreOf, and a driver that already has the store can hand
// it over directly.
var _ agent.DriverStore = (*SQLite)(nil)

type SQLite struct {
	db *sql.DB
	// owned reports whether this store opened db, because Close has to know:
	// a borrowed handle outlives the store that ran on it.
	owned bool
}

var _ agent.Store = (*SQLite)(nil)

// Synchronous is SQLite's commit durability.
type Synchronous int

const (
	// SynchronousFull fsyncs every commit, so a power loss cannot drop one.
	// This is the default.
	SynchronousFull Synchronous = iota
	// SynchronousNormal skips the per-commit fsync. A process crash still
	// recovers every commit; a power loss may lose the last few. A checkpoint is
	// reconstructible from the engine's last durable step, so a host may trade a
	// small replay for a much cheaper write path.
	SynchronousNormal
)

// Option configures a store.
type Option func(*options)

type options struct {
	synchronous Synchronous
}

// WithSynchronous sets the commit durability. The default is SynchronousFull.
func WithSynchronous(mode Synchronous) Option {
	return func(target *options) { target.synchronous = mode }
}

// Open prepares a SQLite store at path. ":memory:" is accepted for tests.
//
// The store owns the handle it opens, and Close ends it. A host that keeps its
// own tables in the same file hands OpenOn the handle instead.
func Open(path string, config ...Option) (*SQLite, error) {
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
	store, err := OpenOn(db, config...)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.owned = true
	return store, nil
}

// OpenOn runs the store on a caller-owned handle, so a host can keep its own
// tables in the same database and back up, export or delete one file. It
// prepares the same store Open does — the pragmas, the additive ALTER tail and
// the schema all run here too, because a caller that had to repeat them would
// re-derive the migration the store exists to own.
//
// Three things the caller answers:
//
// The handle must be pinned to one connection (db.SetMaxOpenConns(1)), and that
// connection must not expire: SetConnMaxLifetime(0), SetConnMaxIdleTime(0) and a
// non-zero SetMaxIdleConns keep database/sql from replacing it, which would
// replace the pragmas with it. The pragmas apply to the connection that ran
// them, so on a pool the store is left with busy_timeout=0 — an immediate
// SQLITE_BUSY instead of the named wait — and a synchronous mode that holds for
// one connection and not the others. OpenOn checks the connection limit and
// refuses rather than reconfiguring a pool its caller set up; the expiry
// settings database/sql does not expose, so they stay the caller's to keep.
//
// The pragmas land on the whole handle, and so on everything else the caller
// writes through it: its commits take the synchronous mode, its writes wait on
// the busy timeout, and foreign_keys=ON applies to its tables as well. That is
// the price of one handle and one commit order, and the reason this is a
// separate constructor rather than an option to Open. A failed OpenOn keeps
// whatever pragmas it already applied: they cannot be rolled back, and on a
// borrowed handle they change the caller's behaviour from then on.
//
// Ownership does not move. Close does nothing here: the caller closes the
// handle, and the store holds no statement of its own to release.
func OpenOn(db *sql.DB, config ...Option) (*SQLite, error) {
	if db == nil {
		return nil, errdefs.Validationf("checkpoint: a database handle is required")
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		return nil, errdefs.Validationf(
			"checkpoint: the handle must be limited to one connection with SetMaxOpenConns(1), got %d", got)
	}
	if err := prepare(db, config); err != nil {
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// prepare applies the pragmas, the additive ALTER tail and the schema to a
// handle that is already open and pinned to one connection.
func prepare(db *sql.DB, config []Option) error {
	var opts options
	for _, option := range config {
		if option != nil {
			option(&opts)
		}
	}
	// The option is resolved before anything below touches the database: a
	// rejected call must not leave the file in a journal mode nobody asked for,
	// which matters most on a handle the caller keeps.
	var durability string
	switch opts.synchronous {
	case SynchronousFull:
		durability = "PRAGMA synchronous=FULL"
	case SynchronousNormal:
		durability = "PRAGMA synchronous=NORMAL"
	default:
		return errdefs.Validationf("checkpoint: unknown synchronous mode %d", opts.synchronous)
	}
	// WAL keeps a reader from blocking the loop's writes. It is a property of
	// the file rather than of the connection, so this is a one-time switch — but
	// the pragma reports the mode it ended on instead of failing, and Exec drops
	// the row: a database that stayed in "delete" (another writer holding it, a
	// filesystem without the shared memory WAL needs) is an error here, not a
	// silent difference later.
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("checkpoint: PRAGMA journal_mode=WAL: %w", err)
	}
	// An in-memory database has no journal file, so it reports "memory" and WAL
	// cannot apply to it: the promise that ":memory:" works is older than this
	// check, and the mode is moot on a database that cannot be reopened.
	if !strings.EqualFold(mode, "wal") && !strings.EqualFold(mode, "memory") {
		return fmt.Errorf("checkpoint: PRAGMA journal_mode=WAL: the database stayed in %q mode "+
			"(WAL needs a filesystem with shared memory, not a network or FUSE mount)", mode)
	}
	// Foreign keys are off by default in SQLite. This schema declares none, so
	// the pragma is inert for these tables — on a shared handle it is not: it
	// turns enforcement on for the caller's tables too.
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		durability,
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("checkpoint: %s: %w", pragma, err)
		}
	}
	// An older database predates the adapter's own counter; adding it here keeps
	// One migration path without a schema-version table: each of these is run
	// unconditionally and a duplicate column is ignored.
	_, _ = db.Exec("ALTER TABLE tasks ADD COLUMN server_version INTEGER NOT NULL DEFAULT 0")
	// The newest task of a session is ordered by this counter, not by the
	// timestamp text: see savedAtLayout.
	_, _ = db.Exec("ALTER TABLE tasks ADD COLUMN saved_at INTEGER NOT NULL DEFAULT 0")
	// A database written before the projection stopped carrying history has no
	// seed column; Load falls back to the body for those rows.
	_, _ = db.Exec("ALTER TABLE tasks ADD COLUMN seed BLOB NOT NULL DEFAULT ''")
	// And one written before events carried a time has no created_at: those rows
	// read back as zero, which a reader treats as unknown rather than as 1970.
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0")
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("checkpoint: migrate: %w", err)
	}
	return nil
}

// Close releases the handle Open created. A store built on a borrowed handle
// closes nothing: the handle is the caller's, and a Close that quietly took it
// away would surface as an error somewhere else entirely. agent.Store has no
// Close, so this stays a property of the concrete backend.
func (l *SQLite) Close() error {
	if !l.owned {
		return nil
	}
	return l.db.Close()
}

// Save updates the projection and appends the round's events in one transaction.
// When the options ask for compaction, the obsolete prefix goes in that same
// transaction: one step, one commit.
func (l *SQLite) Save(ctx context.Context, checkpoint agent.Checkpoint, events []agent.EventRecord, config ...agent.SaveOption) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var options agent.SaveOptions
	for _, option := range config {
		if option != nil {
			option(&options)
		}
	}
	if options.Truncate && options.Through < 0 {
		return 0, 0, errdefs.Validationf("checkpoint: truncation point must not be negative")
	}
	if checkpoint.Task.ID == "" {
		return 0, 0, errdefs.Validationf("checkpoint: task id is required")
	}
	// The seed is the task as of the first save and never changes; the body holds
	// only what changes after it. A transcript in the projection would make every
	// save rewrite the whole conversation, so n rounds cost O(n^2) bytes; it lives in
	// the event log instead.
	seed, err := json.Marshal(checkpoint)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: encode seed: %w", err)
	}
	// The body is the mutable part of the row: the projection plus whatever an
	// adapter above the agent keeps in Sections. The transcript is not here; it lives
	// in the event log.
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

	// Ordering is the save counter's job; updated_at is for a reader. Both are
	// written from one clock read so the row cannot disagree with itself.
	at := time.Now().UTC()
	savedAt := at.UnixNano()
	now := at.Format(savedAtLayout)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (task_id, context_id, state, round, schema, fingerprint, seed, body, version, server_version, saved_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			context_id     = excluded.context_id,
			state          = excluded.state,
			round          = excluded.round,
			schema         = excluded.schema,
			fingerprint    = excluded.fingerprint,
			body           = excluded.body,
			version        = tasks.version + 1,
			server_version = CASE WHEN excluded.server_version > 0
				THEN excluded.server_version ELSE tasks.server_version END,
			saved_at       = excluded.saved_at,
			updated_at     = excluded.updated_at`,
		checkpoint.Task.ID,
		checkpoint.Task.SessionID,
		string(checkpoint.Task.State),
		checkpoint.Task.Round,
		checkpoint.Schema,
		string(checkpoint.Fingerprint),
		seed,
		body,
		checkpoint.ServerVersion,
		savedAt,
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
		if err := appendEvents(ctx, tx, checkpoint.Task.ID, next, events); err != nil {
			return 0, 0, err
		}
	}
	if options.Truncate {
		if err := truncateEvents(ctx, tx, checkpoint.Task.ID, options.Through, options.Kinds...); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: commit: %w", err)
	}
	return first, last, nil
}

// eventInsertRows is how many events one INSERT statement carries. SQLite binds
// at most 999 variables per statement on builds that predate the 32766 raise, and
// each event binds five of them (task_id, seq, kind, payload, created_at), so the
// ceiling is 999/5 = 199 rows. 80 is a round number well under it: 80*5 = 400
// variables.
const eventInsertRows = 80

// appendEvents writes one round's events, starting at seq next, as multi-row
// INSERT statements. Sequences are assigned here, from the caller's first seq and
// the position in events, so a chunk boundary cannot renumber a row.
func appendEvents(ctx context.Context, tx *sql.Tx, taskID string, next int64, events []agent.EventRecord) error {
	// Validated up front: a rejected event must not depend on which chunk it
	// fell into having been written first.
	for i, event := range events {
		if event.Kind == "" {
			return errdefs.Validationf("checkpoint: event %d has no kind", i)
		}
	}
	// One clock read per save: the events of a round share an instant unless the
	// producer named a better one, and a per-event Now() would make one round's
	// facts disagree by however long the loop took.
	saved := time.Now()
	for start := 0; start < len(events); start += eventInsertRows {
		end := min(start+eventInsertRows, len(events))
		chunk := events[start:end]
		var query strings.Builder
		query.WriteString(`INSERT INTO events (task_id, seq, kind, payload, created_at) VALUES `)
		args := make([]any, 0, len(chunk)*5)
		for i, event := range chunk {
			if i > 0 {
				query.WriteString(", ")
			}
			query.WriteString("(?, ?, ?, ?, ?)")
			at := event.At
			if at.IsZero() {
				at = saved
			}
			// A fact may carry no payload — a status or a lifecycle marker is a kind and a
			// time, nothing more — so the column holds an empty blob rather than refusing the
			// row. Untyped nil would reach the driver as NULL and the NOT NULL column would
			// reject a legitimate fact.
			payload := event.Payload
			if len(payload) == 0 {
				payload = emptyPayload
			}
			args = append(args, taskID, next+int64(start+i), event.Kind, []byte(payload), at.UnixNano())
		}
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("checkpoint: append event: %w", err)
		}
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
	checkpoint.Version = version
	checkpoint.ServerVersion = serverVersion
	return checkpoint, nil
}

// LatestForSession returns the newest task of one session.
func (l *SQLite) LatestForSession(ctx context.Context, sessionID string) (agent.Checkpoint, error) {
	var taskID string
	err := l.db.QueryRowContext(ctx,
		`SELECT task_id FROM tasks WHERE context_id = ?
		 ORDER BY saved_at DESC, updated_at DESC LIMIT 1`, sessionID).Scan(&taskID)
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
		`SELECT seq, kind, payload, created_at FROM events WHERE task_id = ? AND seq > ? ORDER BY seq`,
		taskID, after)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]agent.EventRecord, 0, 16)
	for rows.Next() {
		var record agent.EventRecord
		var payload []byte
		var createdAt int64
		if err := rows.Scan(&record.Seq, &record.Kind, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("checkpoint: scan event: %w", err)
		}
		record.Payload = payload
		// Zero is "unknown", not 1970: a row written before events carried a time
		// has no time, and a reader that invented one would place it first on
		// every timeline.
		if createdAt != 0 {
			record.At = time.Unix(0, createdAt).UTC()
		}
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
	// updated_at is for a reader; saved_at is what orders a session's tasks, and
	// this write must not reorder them: the adapter's counter moving is not the
	// engine saving a task.
	res, err := l.db.ExecContext(ctx,
		`UPDATE tasks SET server_version = ?, updated_at = ? WHERE task_id = ?`,
		version, time.Now().UTC().Format(savedAtLayout), taskID)
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
func (l *SQLite) TruncateEvents(ctx context.Context, taskID string, seq int64, kinds ...string) error {
	return truncateEvents(ctx, l.db, taskID, seq, kinds...)
}

// execer is the part of *sql.DB and *sql.Tx truncation needs, so the same guard
// runs whether compaction is its own call or part of a save.
type execer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func truncateEvents(ctx context.Context, db execer, taskID string, seq int64, kinds ...string) error {
	if seq < 0 {
		return errdefs.Validationf("checkpoint: truncation point must not be negative")
	}
	var newest int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE task_id = ?`, taskID,
	).Scan(&newest); err != nil {
		return fmt.Errorf("checkpoint: latest sequence: %w", err)
	}
	if newest == 0 {
		return nil // nothing recorded
	}
	if seq >= newest {
		// The newest fact is never removable: a snapshot names it, and resetting MAX(seq)+1 would
		// let a later save reuse an identifier that snapshot still points at. A caller that asks
		// for more than exists — routine when a compaction computes its point from a projection —
		// gets the tail kept rather than an error, so compaction stays idempotent.
		seq = newest - 1
	}
	query := `DELETE FROM events WHERE task_id = ? AND seq <= ?`
	args := []any{taskID, seq}
	if len(kinds) > 0 {
		placeholders := strings.Repeat(",?", len(kinds))[1:]
		query += " AND kind IN (" + placeholders + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("checkpoint: truncate events: %w", err)
	}
	return nil
}

// List returns every task, newest first.
func (l *SQLite) List(ctx context.Context) ([]agent.TaskSummary, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT task_id, context_id, state, round, updated_at FROM tasks
		 ORDER BY saved_at DESC, updated_at DESC`)
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
