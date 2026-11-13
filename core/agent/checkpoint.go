package agent

import (
	"context"
	"errors"
	"time"

	"github.com/goccy/go-json"
)

// CheckpointSchema versions the checkpoint shape. A reader that does not know
// the schema refuses instead of guessing at fields.
const CheckpointSchema = 1

// ErrNoCheckpoint reports that a store holds nothing to resume from.
var ErrNoCheckpoint = errors.New("agent: no checkpoint")

// Checkpoint is one Task plus the configuration it ran under.
//
// It does not contain a runtime: a runtime holds processes, handles and
// connections that cannot be serialised. What can be is the fingerprint of the
// deployment needed to rebuild one, plus the transcript to replay — which is
// why a checkpoint is a retrieval point for the whole stack.
type Checkpoint struct {
	Schema int `json:"schema"`
	// Version is assigned by the store and advances on every save. It is what a
	// concurrent writer is compared against.
	Version int64 `json:"version,omitempty"`
	// ServerVersion belongs to a protocol adapter (the A2A task store) and only
	// it changes this. The loop's own saves never touch it: two writers sharing
	// one counter would reject each other's writes, and both are legitimate.
	// Zero leaves the stored value untouched, so an engine save cannot reset the
	// adapter's counter.
	ServerVersion int64 `json:"server_version,omitempty"`
	// Fingerprint is the deployment that wrote this checkpoint.
	Fingerprint Fingerprint                `json:"fingerprint"`
	Task        Task                       `json:"task"`
	Sections    map[string]json.RawMessage `json:"sections,omitempty"`
}

// EventRecord is one appended fact of a Task.
//
// The projection (the Task row) answers "what is true now"; these answer "how it
// got there". Storing only the projection makes every write carry the whole
// transcript; storing only events makes every read a replay. Both, in one
// transaction, is what the shape below is for.
//
// A record is ordered by Seq and dated by At, and neither replaces the other: a
// timeline reads them together, an audit reads the payload and the time, and a
// resume reads only the order.
type EventRecord struct {
	// Seq is assigned by the store, monotonic per task.
	Seq int64
	// Kind names the fact: "message", "state", "artifact".
	Kind string
	// Payload is the fact itself.
	Payload json.RawMessage
	// At is when the fact happened. Zero asks the store to stamp the save, so a
	// producer with no better answer still gets a time.
	//
	// The anchor is per kind, because the facts are not all instants:
	//
	//	graph.state, graph.frontier, graph.base  the save that wrote them
	//	llm.call                                the call's start;
	//	                                        end = At + LatencyMs
	//	graph.effect (started)                  the call's start
	//	graph.effect (done, failed)             the call's end;
	//	                                        start = At - DurationMs
	//	script.run, script.tool                 the invocation's start
	//	message                                 the arrival of the message
	//
	// It is not a replacement for Seq: Seq orders facts, At dates them, and a
	// store keeps both. A reader must treat a zero At as unknown rather than as
	// 1970 — a row written before this field existed has no time at all.
	At time.Time
}

// TaskSummary is the list projection.
type TaskSummary struct {
	ID        string
	SessionID string
	State     TaskState
	Round     int
	UpdatedAt string
}

// SaveOption configures one Save.
type SaveOption func(*SaveOptions)

// SaveOptions is the resolved form of a Save's options.
type SaveOptions struct {
	// Truncate asks the save to compact the log as well.
	Truncate bool
	// Through is the last sequence the save may drop; only meaningful with
	// Truncate.
	Through int64
	// Kinds limits truncation to these event kinds. Empty means every kind,
	// which is the pre-trace behaviour.
	Kinds []string
}

// TruncateThrough asks Save to drop the events at or before seq in the same
// transaction that writes this round. Compaction belongs in the save that makes
// the base durable: doing it as a second call pays two commits for one decision.
//
// A store still refuses to drop the newest event, and TruncateEvents remains the
// standalone form for a host that compacts out of band.
func TruncateThrough(seq int64, kinds ...string) SaveOption {
	return func(options *SaveOptions) {
		options.Truncate = true
		options.Through = seq
		options.Kinds = kinds
	}
}

// Fingerprint is the identity a checkpoint records: the deployment that produced
// it. A resume compares it, so a task cannot continue under a runtime, engine or
// protocol different from the one it was written under.
//
// It is opaque here: deploy composes it from the runtime's own digest and the
// engine and protocol identities, and a store only persists the string.
type Fingerprint string

// Store persists tasks, their events, and the blobs they reference.
//
// The interface is semantic rather than relational: a task is addressed by id,
// and a first save is just a save, so create and append collapse into Save.
// Where state lives — a local file, Postgres, an object store — is a deployment
// decision, so the implementation stays replaceable.
type Store interface {
	// Save writes the Task projection and appends this round's events in one
	// transaction. The first save creates the task; later ones advance it.
	//
	// The projection never carries history: Task.History is ignored, because the
	// event log is its only source. Save returns the sequence range it assigned
	// to the events (first, last), or (0, 0) when there were none, so a caller can
	// point at an event it just wrote without a second query.
	//
	// Options may also compact the log: TruncateThrough drops what a new base
	// already covers, in the same transaction.
	Save(ctx context.Context, checkpoint Checkpoint, events []EventRecord, options ...SaveOption) (first, last int64, err error)
	// Load returns the newest projection of one task.
	//
	// It does not replay the event log: Task.History is always empty. A caller
	// that needs history reads Events or EventsAfter. Keeping the projection
	// small is what makes Load cost the same for a one-turn task and a
	// thousand-turn one.
	Load(ctx context.Context, taskID string) (Checkpoint, error)
	// LatestForSession returns the newest task of one session, which is
	// what resuming a session needs: checkpoints are keyed by task, but a
	// session outlives each of its tasks.
	LatestForSession(ctx context.Context, sessionID string) (Checkpoint, error)
	// Events returns every appended fact of one task, oldest first.
	Events(ctx context.Context, taskID string) ([]EventRecord, error)
	// EventsAfter returns the facts recorded after seq (exclusive), oldest first.
	// It is what a restore reads when a snapshot already covers everything up to
	// that point.
	EventsAfter(ctx context.Context, taskID string, after int64) ([]EventRecord, error)
	// SetServerVersion advances the adapter's own counter without rewriting the
	// projection. A protocol adapter that records one event per message uses it:
	// the engine owns the state, and the adapter owns only its token.
	SetServerVersion(ctx context.Context, taskID string, version int64) error
	// TruncateEvents removes the events at or before seq.
	//
	// It is the caller's invariant that a later snapshot covers them; a store
	// cannot tell an obsolete event from a missing one. The newest event is never
	// removable, so sequence numbers stay monotonic and a later Save continues
	// after the tail rather than reusing a number.
	//
	// Kinds limits the deletion to those event kinds: an engine that keeps
	// trace records beside its state compacts only the state, so the records
	// survive. Empty means every kind.
	TruncateEvents(ctx context.Context, taskID string, seq int64, kinds ...string) error
	// List returns every task, newest first.
	List(ctx context.Context) ([]TaskSummary, error)
	// Delete removes a task and its events.
	Delete(ctx context.Context, taskID string) error
	// PutBlob stores bytes and returns their reference. The same bytes must
	// yield the same reference, so a repeated write costs nothing.
	PutBlob(ctx context.Context, data []byte) (string, error)
	// GetBlob returns the bytes a reference names.
	GetBlob(ctx context.Context, ref string) ([]byte, error)
}
