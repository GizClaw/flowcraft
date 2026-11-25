package agent

import (
	"errors"

	"github.com/GizClaw/flowcraft/core/message"
)

// Steering is the operator's channel into a turn in flight.
//
// It is a distinct feature from cancellation, not a variant of it. Cancel discards;
// steering continues with new information — an operator who sees a run going the wrong
// way redirects it, a graph inserts the input after the step it was waiting on. The
// A2A protocol has no steering, which is a limitation of that protocol: an adapter for
// it simply does not implement [Steerer].
//
// # The undo window
//
// An input is queued, then applied by the driver at a step boundary, then history. The
// window between queued and applied is the only window in which it can be withdrawn:
// once the driver has inserted the message into the transcript, revoking it would mean
// rewriting a conclusion the model already acted on. After that, the recourse is a new
// steer — which is itself an audited event.

// SteerIntent says why an input was injected, so the record explains what changed.
type SteerIntent string

const (
	// SteerRedirect changes where the turn is going.
	SteerRedirect SteerIntent = "steer"
	// SteerCorrect fixes a premise the run was built on.
	SteerCorrect SteerIntent = "inject"
	// SteerDeliver reports work that arrived from elsewhere — a delegated run that finished, or
	// a subagent asking the agent that delegated to it. It is not an operator's voice, and the
	// record keeps the difference.
	SteerDeliver SteerIntent = "deliver"
)

// Valid reports whether the intent is one of the three.
func (s SteerIntent) Valid() bool {
	return s == SteerRedirect || s == SteerCorrect || s == SteerDeliver
}

// SteerID identifies one queued input, so it can be withdrawn before it is applied.
type SteerID string

// SteerInput is one message offered to a turn in flight.
type SteerInput struct {
	Message message.Message
	Intent  SteerIntent
}

var (
	// ErrSessionIdle reports that the session has no turn in flight. The host answers
	// this one: it owns the table. An input offered now would be a new message, and a
	// new message goes through Run.
	ErrSessionIdle = errors.New("agent: the session has no run in flight")
	// ErrRunEnded reports that the run finished before the input was applied. The
	// driver answers this one: only it knows it has taken its last step.
	ErrRunEnded = errors.New("agent: the run already ended")
	// ErrNotSteerable reports that this driver cannot take input at a boundary. It is
	// refused at the call site rather than queued into the void.
	ErrNotSteerable = errors.New("agent: this driver cannot be steered")
	// ErrNotCancellable reports that this driver cannot forward a cancellation to a
	// peer. Local drivers are cancelled through the context and never report this.
	ErrNotCancellable = errors.New("agent: this driver cannot forward a cancellation")
	// ErrSteerApplied reports that a queued input was already applied, so withdrawing
	// it would rewrite history.
	ErrSteerApplied = errors.New("agent: the input was already applied")
	// ErrSteerUnknown reports a handle that names nothing: already applied and
	// forgotten, or never queued here.
	ErrSteerUnknown = errors.New("agent: unknown steer id")
)

// Interruption is what a driver applied at a boundary.
//
// It carries the intent and the position, not the text: the applied message is in the
// transcript and in the audit record, and duplicating it in every reader's memory is
// how a stream grows without bound.
type Interruption struct {
	// ID is the handle Steer returned, so a caller that queued several inputs can tell which
	// one reached a boundary.
	ID       SteerID     `json:"id,omitempty"`
	Intent   SteerIntent `json:"intent"`
	Boundary int         `json:"boundary,omitempty"`
}

// InputSink is the optional half of a [Driver] that can take a message from outside its turn.
//
// A driver that cannot — one whose document declares no input slot — would accept the message
// and never show it to the model, which is worse than refusing it: the sender believes the work
// was delivered. The host asks before it starts a turn for work that arrived while the session
// was idle, and holds the message for the application when the answer is no.
type InputSink interface {
	AcceptsInput() bool
}
