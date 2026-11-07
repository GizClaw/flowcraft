package agent

import (
	"context"
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
	Schema   int                        `json:"schema"`
	Runtime  runtime.Digest             `json:"runtime"`
	Task     Task                       `json:"task"`
	Sections map[string]json.RawMessage `json:"sections,omitempty"`
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
	return Checkpoint{
		Schema:  CheckpointSchema,
		Runtime: s.runtime.Config().Digest(),
		Task:    *s.task,
	}
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
	if want := rt.Config().Digest(); checkpoint.Runtime != want {
		return nil, errdefs.Validationf(
			"agent: checkpoint was taken under %s, this runtime is %s",
			checkpoint.Runtime, want)
	}
	session := &Session{runtime: rt, task: &checkpoint.Task}
	for _, opt := range opts {
		if opt != nil {
			opt(&session.opts)
		}
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
