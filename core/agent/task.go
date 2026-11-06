package agent

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// TaskState is the lifecycle of one Task. The names are the A2A task states, so
// an adapter does not have to translate them.
type TaskState string

const (
	// StateSubmitted means the Task exists and has not produced anything yet.
	StateSubmitted TaskState = "submitted"
	// StateWorking means the loop is running.
	StateWorking TaskState = "working"
	// StateInputRequired means the Task stopped to ask the user something, and
	// can be resumed with the answer.
	StateInputRequired TaskState = "input-required"
	// StateCompleted means the Task produced its final answer.
	StateCompleted TaskState = "completed"
	// StateFailed means the loop stopped on an error.
	StateFailed TaskState = "failed"
	// StateCanceled means the caller interrupted the Task or its context ended.
	StateCanceled TaskState = "canceled"
	// StateRejected means a precondition refused the Task before any model call.
	StateRejected TaskState = "rejected"
	// StateAuthRequired means the Task needs credentials it does not have.
	StateAuthRequired TaskState = "auth-required"
)

// Terminal reports whether no further work happens without a new Run.
func (s TaskState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// Artifact is a reference to something the Task produced. The bytes live in a
// blob store; a checkpoint stays small because this record carries only the
// reference.
type Artifact struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Blob      string `json:"blob"`
}

// Task is one user request and everything the loop accumulated answering it.
//
// It is the unit a checkpoint captures: the transcript, the system prompt that
// framed it, how far it got, and what it produced. Per-round detail (which
// model, how many tokens, how long) is telemetry, not state, so it is not here.
type Task struct {
	ID        string            `json:"id"`
	ContextID string            `json:"context_id,omitempty"`
	State     TaskState         `json:"state"`
	Round     int               `json:"round"`
	System    message.Content   `json:"system,omitzero"`
	History   []message.Message `json:"history,omitempty"`
	Artifacts []Artifact        `json:"artifacts,omitempty"`
}

// newTaskID returns an opaque id. It is stable across resumptions because a
// restored Task keeps the id it was checkpointed with.
func newTaskID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "task"
	}
	return "task-" + hex.EncodeToString(buf[:])
}

// advance moves the Task to its next state, refusing an illegal transition.
func (t *Task) advance(state TaskState) error {
	if state == t.State {
		return nil
	}
	if t.State.Terminal() {
		return errdefs.Validationf("agent: task %s is %s and cannot become %s",
			t.ID, t.State, state)
	}
	t.State = state
	return nil
}
