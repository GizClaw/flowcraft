package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// CheckpointSchema versions the checkpoint shape. A reader that does not know
// the schema refuses instead of guessing at fields.
const CheckpointSchema = 1

// ErrNoCheckpoint reports that a store holds nothing to resume from.
var ErrNoCheckpoint = errors.New("agent: no checkpoint")

// Checkpoint is one Task plus the configuration it ran under.
//
// It does not contain a runtime: a runtime holds processes, handles and
// connections that cannot be serialised. What can be is the digest of the
// configuration needed to rebuild one, plus the transcript to replay — which is
// why a checkpoint is a retrieval point for the whole stack.
type Checkpoint struct {
	Schema int `json:"schema"`
	// Version is assigned by the store and advances on every save. It is what a
	// concurrent writer is compared against.
	Version int64 `json:"version,omitempty"`
	// ServerVersion belongs to a protocol adapter (the A2A task store) and only
	// it changes this. The loop's own saves never touch it: two writers sharing
	// one counter would reject each other's writes, and both are legitimate.
	ServerVersion int64                      `json:"server_version,omitempty"`
	Runtime       runtime.Digest             `json:"runtime"`
	Task          Task                       `json:"task"`
	Sections      map[string]json.RawMessage `json:"sections,omitempty"`
}

// EventRecord is one appended fact of a Task.
//
// The projection (the Task row) answers "what is true now"; these answer "how it
// got there". Storing only the projection makes every write carry the whole
// transcript; storing only events makes every read a replay. Both, in one
// transaction, is what the shape below is for.
type EventRecord struct {
	// Seq is assigned by the store, monotonic per task.
	Seq int64
	// Kind names the fact: "message", "state", "artifact".
	Kind string
	// Payload is the fact itself.
	Payload json.RawMessage
}

// TaskSummary is the list projection.
type TaskSummary struct {
	ID        string
	ContextID string
	State     TaskState
	Round     int
	UpdatedAt string
}

// Store persists tasks, their events, and the blobs they reference.
//
// The verbs follow the session-service shape the ecosystem converged on
// (create/append/list/delete plus a projection), collapsed to one Save because a
// Task is addressed by id and a first save is just a save. Where state lives — a
// local file, Postgres, an object store — is a deployment decision, so the
// interface stays semantic and the implementation is replaceable.
type Store interface {
	// Save writes the Task projection and appends this round's events in one
	// transaction. The first save creates the task; later ones advance it.
	Save(ctx context.Context, checkpoint Checkpoint, events []EventRecord) error
	// Load returns the newest projection of one task.
	Load(ctx context.Context, taskID string) (Checkpoint, error)
	// LatestForContext returns the newest task of one conversation, which is
	// what resuming a conversation needs: checkpoints are keyed by task, but a
	// conversation outlives each of its tasks.
	LatestForContext(ctx context.Context, conversationID string) (Checkpoint, error)
	// Events returns the appended facts of one task, oldest first.
	Events(ctx context.Context, taskID string) ([]EventRecord, error)
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

// Snapshot captures the Session's current Task.
func (s *Session) Snapshot() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The task is cloned: a checkpoint handed to an asynchronous store must not
	// change when the loop appends to History afterwards.
	return Checkpoint{
		Schema:  CheckpointSchema,
		Runtime: s.resumeDigest(),
		Task:    s.task.clone(),
	}
}

// resumeDigest is the identity a checkpoint records and a restore compares.
func (s *Session) resumeDigest() runtime.Digest {
	base := s.runtime.Config().Digest()
	if s.opts.Digest == "" {
		return base
	}
	// An override extends the runtime's identity; replacing it would let a
	// caller resume under a different model or permission set by supplying a
	// constant.
	sum := sha256.Sum256([]byte(string(base) + "\x00" + string(s.opts.Digest)))
	return runtime.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// Restore resumes a Session from a checkpoint.
//
// The digest must match the runtime it is resumed against. Continuing a
// transcript under a different model, boundary or permission set is the most
// dangerous silent failure this layer has: everything the model was told about
// what it may do came from the old configuration.
func Restore(rt *runtime.Runtime, checkpoint Checkpoint, opts ...Option) (*Session, error) {
	if rt == nil {
		return nil, errdefs.Validationf("agent: a runtime is required")
	}
	if checkpoint.Schema != CheckpointSchema {
		return nil, errdefs.Validationf(
			"agent: checkpoint schema %d is not supported; this build reads %d",
			checkpoint.Schema, CheckpointSchema)
	}
	session := &Session{runtime: rt, task: &checkpoint.Task}
	for _, opt := range opts {
		if opt != nil {
			opt(&session.opts)
		}
	}
	if want := session.resumeDigest(); checkpoint.Runtime != want {
		return nil, errdefs.Validationf(
			"agent: checkpoint was taken under %s, this runtime is %s",
			checkpoint.Runtime, want)
	}
	return session, nil
}

// RestoreFrom loads a task from a store and resumes it.
func RestoreFrom(ctx context.Context, rt *runtime.Runtime, store Store, taskID string, opts ...Option) (*Session, error) {
	if store == nil {
		return nil, errdefs.Validationf("agent: a store is required")
	}
	checkpoint, err := store.Load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return Restore(rt, checkpoint, opts...)
}
