package agent

import (
	"context"
	"iter"

	"github.com/GizClaw/flowcraft/core/message"
)

// Runner is what an engine implements.
//
// It is deliberately small: advance a Task, cancel it, and release whatever it
// holds. There is one entry, not two: a message is either the request that
// starts a Task or the answer to the question it paused on, and only the engine
// knows which — its own state says so. A caller never has to.
//
// It owns no mailbox and no queue: a message arrives in the call that handles it,
// and a session that already has a run in flight is refused rather than
// buffered. That is the protocol's model too — a message is a request, not an
// append to a backlog.
//
// A runner reports every state transition as a Status event, including the
// terminal one. The Agent keeps its table in step with those events, so a
// runner that stays silent leaves State reporting where the Task was, not where
// it is.
type Runner interface {
	// State reports where the Task stands.
	State() TaskState
	// Run handles one message. A Task that is waiting for input continues with
	// it; any other Task starts from it.
	Run(ctx context.Context, msg message.Message) iter.Seq2[Event, error]
	// Cancel stops the Task. A runner that has already finished reports nil.
	Cancel(ctx context.Context, reason string) error
	// Close releases the runner. The Task's state lives in the store, so a
	// closed runner can be rebuilt and resumed.
	Close() error
}

// Steerer is an optional engine capability: a message applied to the run in
// flight at its next step boundary.
//
// It is not part of Runner because most engines have nothing to apply it to, and
// an engine that cannot must say so instead of dropping the message. Steering is
// also not an answer: a Task waiting for input is idle, and its answer goes
// through Run.
type Steerer interface {
	// Steer hands msg to the running Task and returns once the Task has taken
	// it. A Task that ends first reports ErrRunEnded, so the caller knows the
	// message was not applied and can send it again.
	//
	// The engine decides what "taken" means: an engine that queues to a step
	// boundary returns as soon as the message is queued, and reports the
	// applied boundary in the Interrupted event.
	Steer(ctx context.Context, msg message.Message, kind InterruptKind) (Interruption, error)
}
