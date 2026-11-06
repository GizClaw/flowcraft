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

// Checkpoint is a resume point: the conversation, and the configuration it ran
// under.
//
// It does not contain a runtime. A runtime holds processes, handles and
// connections that cannot be serialised; what can be is the digest of the
// configuration needed to rebuild one, plus the transcript to replay. That is
// why a checkpoint is a retrieval point for the whole stack rather than a
// private detail of the loop.
type Checkpoint struct {
	Schema   int                        `json:"schema"`
	Runtime  runtime.Digest             `json:"runtime"`
	Task     Task                       `json:"task"`
	Sections map[string]json.RawMessage `json:"sections,omitempty"`
}

// Store persists checkpoints and the blobs they reference.
//
// It is the host's to implement, for the same reason drivers and secret
// resolvers are: where state lives — a local file, an object store, a database
// per tenant — is a deployment decision, not a framework one. The methods are
// deliberately at the semantic level; append-only logs and pointers are an
// implementation choice, not part of the contract.
type Store interface {
	// Write persists one checkpoint. It does not need to be durable on return:
	// the loop does not wait for it.
	Write(ctx context.Context, checkpoint Checkpoint) error
	// Latest returns the most recent checkpoint.
	Latest(ctx context.Context) (Checkpoint, error)
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
// what it may do came from the old configuration. A caller that genuinely wants
// to drift has to say so; there is no default that guesses.
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
