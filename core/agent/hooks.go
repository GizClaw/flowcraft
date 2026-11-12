package agent

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Hooks are the host's policy at the boundaries of a turn.
//
// The three points are the ones a host owns, rather than the ones an engine owns:
// a session arrives, a turn arrives, a turn ends. They exist because a host has no
// other place to reach a turn it did not start - a delegated child, a message that
// came in over A2A - and because "what this agent is" belongs to the agent, not to
// the substrate that happens to run it.
//
// What a hook cannot do is deliberate. It cannot rewrite the system framing, cut
// the engine's transcript, or intercept one model call: those are the engine's
// state, and a hook that reached into them would turn this package into a prompt
// framework.
type Hooks struct {
	// OnSession runs after a session is opened or restored, before its turn. An
	// error fails the open, so a host that cannot prepare its memory refuses the
	// turn instead of running it blind.
	OnSession func(ctx context.Context, session SessionInfo) error
	// BeforeTurn runs before the turn is handed to the engine and may rewrite its
	// message: a seat's recap, its private memory, the identity it speaks as. An
	// error stops the turn before it starts.
	BeforeTurn func(ctx context.Context, turn *Turn) error
	// AfterTurn runs after the run ends, with the state and result it reached.
	//
	// A failure is logged and does not change the turn: the work is already
	// durable, and reporting an error after the terminal event would break the
	// stream contract every consumer relies on.
	AfterTurn func(ctx context.Context, turn Turn, result Result) error
}

// SessionInfo is what OnSession is told about the session it is entering.
type SessionInfo struct {
	// Agent is the deployment name: the seat a host policy is bound to.
	Agent string
	// SessionID and TaskID identify the session and the task it continues.
	SessionID string
	TaskID    string
	// State is where the task stands: submitted for a new one, or wherever a
	// restored one was left.
	State TaskState
	// Resumed reports that the task came from the store rather than being minted.
	Resumed bool
}

// Turn is one message and the identity it is handled under.
type Turn struct {
	// Agent is the deployment name.
	Agent string
	// SessionID is the session the message belongs to.
	SessionID string
	// TaskID is empty in BeforeTurn: the task is minted by the run it starts.
	TaskID string
	// Message is the turn's input. BeforeTurn may replace it, including its
	// metadata.
	Message message.Message
}

// Empty reports whether no hook is set, so the run path can skip the plumbing.
func (h Hooks) Empty() bool {
	return h.OnSession == nil && h.BeforeTurn == nil && h.AfterTurn == nil
}

// Chain composes hooks into one: each stage runs in declaration order.
//
// OnSession and BeforeTurn stop at the first failure, because a later hook would
// be acting on a session or a message its predecessor rejected. AfterTurn runs
// every hook and logs what failed: one policy's bookkeeping is not another's to
// cancel.
func Chain(hooks ...Hooks) Hooks {
	var chained Hooks
	var sessions, before, after []int
	for i, hook := range hooks {
		if hook.OnSession != nil {
			sessions = append(sessions, i)
		}
		if hook.BeforeTurn != nil {
			before = append(before, i)
		}
		if hook.AfterTurn != nil {
			after = append(after, i)
		}
	}
	if len(sessions) > 0 {
		chained.OnSession = func(ctx context.Context, session SessionInfo) error {
			for _, i := range sessions {
				if err := hooks[i].OnSession(ctx, session); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if len(before) > 0 {
		chained.BeforeTurn = func(ctx context.Context, turn *Turn) error {
			for _, i := range before {
				if err := hooks[i].BeforeTurn(ctx, turn); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if len(after) > 0 {
		chained.AfterTurn = func(ctx context.Context, turn Turn, result Result) error {
			for _, i := range after {
				if err := hooks[i].AfterTurn(ctx, turn, result); err != nil {
					telemetry.WarnErr(ctx, "agent: an after-turn hook failed", err)
				}
			}
			return nil
		}
	}
	return chained
}
