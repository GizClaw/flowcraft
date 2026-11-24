// What a run leaves behind: the task projection, the event log beside it, and the
// contract a store keeps them under.
//
// A task is what survives a process; a turn is what happens inside one. The two shapes
// live in one package because they are one concept — a turn that could not be resumed
// would not need a checkpoint, and a checkpoint that did not describe a turn would have
// nothing to store.
//
// # Two shapes, one task
//
// The projection answers "what is true now": state, round, transcript, artifacts.
// The event log answers "how it got there". Storing only the projection makes every
// write carry the whole transcript; storing only events makes every read a replay.
// Both, in one transaction, is what [Store.Save] is for.
//
// # What is deliberately absent
//
// No runtime, no handles, no connections: none of them serialise. A checkpoint
// records the [Fingerprint] of the deployment that wrote it, so a resume can refuse to
// continue under a different one instead of silently mixing two.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// Schema versions the checkpoint shape. A reader that does not know the schema
// refuses instead of guessing at fields.
const Schema = 1

// ErrEffectUncertain reports that a recorded effect started and never finished.
//
// A resume cannot know whether the side effect happened: the process died between the "started"
// record and its outcome. Guessing either way is worse than refusing, because one guess repeats an
// effect and the other skips one.
var ErrEffectUncertain = errors.New("agent: an effect was started and never finished")

// ErrNoCheckpoint reports that a store holds nothing to resume from.
var ErrNoCheckpoint = errors.New("checkpoint: no checkpoint")

// TaskState is the lifecycle of one task. The names are the A2A task states, so a
// protocol adapter does not have to translate them.
type TaskState string

const (
	// StateSubmitted means the task exists and has not produced anything yet.
	StateSubmitted TaskState = "submitted"
	// StateWorking means the run is in progress.
	StateWorking TaskState = "working"
	// StateInputRequired means the task stopped to ask the user something and can be
	// resumed with the answer.
	StateInputRequired TaskState = "input-required"
	// StateCompleted means the task produced its final answer.
	StateCompleted TaskState = "completed"
	// StateFailed means the run stopped on an error.
	StateFailed TaskState = "failed"
	// StateCanceled means the caller interrupted the task or its context ended.
	StateCanceled TaskState = "canceled"
	// StateRejected means a precondition refused the task before any model call.
	StateRejected TaskState = "rejected"
	// StateAuthRequired means the task needs credentials it does not have.
	StateAuthRequired TaskState = "auth-required"
)

// Valid reports whether s is one of the task states.
func (s TaskState) Valid() bool {
	switch s {
	case StateSubmitted, StateWorking, StateInputRequired, StateCompleted,
		StateFailed, StateCanceled, StateRejected, StateAuthRequired:
		return true
	default:
		return false
	}
}

// Terminal reports whether no further work happens without a new run.
func (s TaskState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// Artifact is a reference to something the task produced. The bytes live in a blob
// store; a checkpoint stays small because this record carries only the reference.
type Artifact struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Blob      string `json:"blob"`
}

// Task is one user request and everything the run accumulated answering it.
//
// It is the unit a checkpoint captures: the transcript, how far it got, and what it
// produced. A system prompt is a message in History, not a field beside it.
// Per-round detail (which model, how many tokens, how long) is telemetry, not state,
// so it is not here.
type Task struct {
	ID        string            `json:"id"`
	SessionID string            `json:"context_id,omitempty"`
	State     TaskState         `json:"state"`
	Round     int               `json:"round"`
	History   []message.Message `json:"history,omitempty"`
	Artifacts []Artifact        `json:"artifacts,omitempty"`
	// Pending is the tool call the task stopped on when it needed the user. It is
	// checkpointed so a resume knows which call the answer belongs to.
	Pending *message.ToolCall `json:"pending,omitempty"`
}

// Checkpoint is one task plus the configuration it ran under.
type Checkpoint struct {
	Schema int `json:"schema"`
	// Version is assigned by the store and advances on every save. It is what a
	// concurrent writer is compared against.
	Version int64 `json:"version,omitempty"`
	// ServerVersion belongs to a protocol adapter and only it changes this. The
	// run's own saves never touch it: two writers sharing one counter would reject
	// each other's writes, and both are legitimate. Zero leaves the stored value
	// untouched.
	ServerVersion int64 `json:"server_version,omitempty"`
	// Fingerprint is the deployment that wrote this checkpoint.
	Fingerprint Fingerprint                `json:"fingerprint"`
	Task        Task                       `json:"task"`
	Sections    map[string]json.RawMessage `json:"sections,omitempty"`
}

// EventRecord is one appended fact of a task.
//
// A record is ordered by Seq and dated by At, and neither replaces the other: a
// timeline reads them together, an audit reads the payload and the time, and a resume
// reads only the order.
type EventRecord struct {
	// Seq is assigned by the store, monotonic per task.
	Seq int64
	// Kind names the fact: "message", "state", "artifact".
	Kind string
	// Payload is the fact itself.
	Payload json.RawMessage
	// At is when the fact happened. Zero asks the store to stamp the save, so a
	// producer with no better answer still gets a time. A reader must treat zero as
	// unknown rather than as 1970.
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

// Fingerprint is the identity a checkpoint records: the deployment that produced it.
// A resume compares it, so a task cannot continue under a runtime or driver different
// from the one it was written under.
//
// It is opaque here: the host composes it from the deployment set it was given — agent
// names, program kinds, grant digests and hook kinds — and a store only persists the
// string.
type Fingerprint string

// SaveOption configures one Save.
type SaveOption func(*SaveOptions)

// SaveOptions is the resolved form of a save's options.
type SaveOptions struct {
	// Truncate asks the save to compact the log as well.
	Truncate bool
	// Through is the last sequence the save may drop; only meaningful with Truncate.
	Through int64
	// Kinds limits truncation to these event kinds. Empty means every kind.
	Kinds []string
}

// TruncateThrough asks Save to drop the events at or before seq in the same
// transaction that writes this round. Compaction belongs in the save that makes the
// base durable: doing it as a second call pays two commits for one decision.
//
// A store still refuses to drop the newest event, so sequence numbers stay monotonic.
func TruncateThrough(seq int64, kinds ...string) SaveOption {
	return func(options *SaveOptions) {
		options.Truncate = true
		options.Through = seq
		options.Kinds = kinds
	}
}

// Store persists tasks, their events, and the blobs they reference.
//
// The interface is semantic rather than relational: a task is addressed by id, and a
// first save is just a save, so create and append collapse into Save. Where state
// lives — a local file, Postgres, an object store — is a deployment decision, so the
// implementation stays replaceable. The conformance suite lives in agent/agenttest.
type Store interface {
	// Save writes the task projection and appends this round's events in one
	// transaction. The first save creates the task; later ones advance it.
	//
	// The projection never carries history: Task.History is ignored, because the
	// event log is its only source. Save returns the sequence range it assigned to
	// the events (first, last), or (0, 0) when there were none.
	//
	// Sections merge rather than replace: a key this save omits keeps the value the
	// previous save wrote. That is what "owner-keyed" means — the manifest is written
	// once by the first save and never again, and a driver that stops publishing a
	// section is not deleting it.
	Save(ctx context.Context, cp Checkpoint, events []EventRecord, options ...SaveOption) (first, last int64, err error)
	// Load returns the newest projection of one task.
	//
	// It does not replay the event log: Task.History is always empty. A caller that
	// needs history reads Events or EventsAfter. Keeping the projection small is what
	// makes Load cost the same for a one-turn task and a thousand-turn one.
	Load(ctx context.Context, taskID string) (Checkpoint, error)
	// LatestForSession returns the newest task of one session, which is what resuming
	// a session needs: checkpoints are keyed by task, but a session outlives each of
	// its tasks.
	LatestForSession(ctx context.Context, sessionID string) (Checkpoint, error)
	// Events returns every appended fact of one task, oldest first.
	Events(ctx context.Context, taskID string) ([]EventRecord, error)
	// EventsAfter returns the facts recorded after seq (exclusive), oldest first.
	EventsAfter(ctx context.Context, taskID string, after int64) ([]EventRecord, error)
	// SetServerVersion advances an adapter's own counter without rewriting the
	// projection.
	SetServerVersion(ctx context.Context, taskID string, version int64) error
	// TruncateEvents removes the events at or before seq.
	//
	// It is the caller's invariant that a later snapshot covers them; a store cannot
	// tell an obsolete event from a missing one. The newest event is never removable.
	// Kinds limits the deletion to those event kinds.
	TruncateEvents(ctx context.Context, taskID string, seq int64, kinds ...string) error
	// List returns every task, newest first.
	List(ctx context.Context) ([]TaskSummary, error)
	// Delete removes a task and its events.
	Delete(ctx context.Context, taskID string) error
	// PutBlob stores bytes and returns their reference. The same bytes must yield the
	// same reference, so a repeated write costs nothing.
	PutBlob(ctx context.Context, data []byte) (string, error)
	// GetBlob returns the bytes a reference names.
	GetBlob(ctx context.Context, ref string) ([]byte, error)
}

// Saver is what a driver sees of a store: append facts, and read where the task
// stands. It is the narrow half of [Store], so a driver cannot compact the log or
// delete a task.
type Saver interface {
	// Append writes the facts and returns the sequence range they were given.
	Append(ctx context.Context, taskID string, events []EventRecord) (first, last int64, err error)
	// Load returns the newest projection, for a driver that resumes mid-task.
	Load(ctx context.Context, taskID string) (Checkpoint, error)
}

// ManifestSchema versions the manifest's shape and digest inputs.
//
// A manifest id is written into a checkpoint and into every run record, so two runs are
// comparable exactly when their ids match. Changing how an id is computed without
// changing this string would silently make old and new ids incomparable under one name;
// with it, a new algorithm is a new schema, and a reader can tell the two apart.
const ManifestSchema = "manifest/1"

// Manifest is the version combination a run executed under.
//
// It answers "which deployment, driver, model catalog and tool schema produced this
// trace" without joining every configuration store. The values are opaque hashes: two
// runs under the same manifest are comparable, and the per-call record still names the
// model that actually answered.
//
// Every field names its producer, and a field nobody fills is not a field: the host
// assembles this once per deployment, at install, and states which facts it can answer
// instead of leaving a reader to guess that an empty one means "not applicable".
//
// What is deliberately absent is a prompt: this framework has no prompt bundle to
// identify. A driver's framing is a message in the transcript, and the identity that
// matters — which messages entered which call — is recorded per call, in
// [LLMCallRecord.Messages].
type Manifest struct {
	// Schema versions the shape and the digest inputs. See [ManifestSchema].
	Schema string `json:"schema,omitempty"`
	// Deployment is the checkpoint fingerprint of the deployment set.
	Deployment Fingerprint `json:"deployment,omitempty"`
	// Driver is the kind the deployment selected ("graph", "a2a").
	Driver string `json:"driver,omitempty"`
	// Models is the model catalog this deployment may call, as it was declared.
	Models string `json:"models,omitempty"`
	// Tools is the tool schema the deployment exposes: the definitions the model reads,
	// after the grant's exposure and any decorator.
	Tools string `json:"tools,omitempty"`
	// Harness is the host-supplied build identity.
	Harness string `json:"harness,omitempty"`
}

// ID is the manifest's identity: a stable hash of its fields. A zero manifest has no
// identity, which is the safe answer for a run that recorded none.
func (m Manifest) ID() string {
	if m == (Manifest{}) {
		return ""
	}
	body, err := json.Marshal(m)
	if err != nil {
		// Every field is plain data; a marshal failure cannot happen, and an empty id
		// is the safe direction.
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
