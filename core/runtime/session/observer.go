package session

import "github.com/GizClaw/flowcraft/core/agent"

// SessionObserver observes session-level lifecycle transitions without
// affecting their outcome. It is the public hook for metrics, tracing, and
// accounting that wants the session view (Key, RunID, terminal outcome)
// rather than per-run agent events.
//
// Concurrency contract:
//
//   - Every method is called synchronously from the goroutine performing
//     the transition, outside Session and Manager locks.
//   - Implementations must return promptly and MUST NOT call back into
//     Session or Manager methods; long-running side effects must be
//     dispatched asynchronously by the observer itself.
//   - Implementations must be safe for concurrent use: SessionStarted and
//     TurnFinished can be called from a turn goroutine while Closing and
//     Closed run on a close/reclaim goroutine.
//
// Embed [BaseSessionObserver] to satisfy the interface with no-op defaults
// when only a subset of the lifecycle is interesting.
type SessionObserver interface {
	// OnSessionStarted fires after Start successfully scheduled a new turn
	// and before Start returns to its caller. turn is the running turn.
	OnSessionStarted(session *Session, turn *Turn)

	// OnSessionClosing fires exactly once when the Session begins closing,
	// covering explicit close, idle timeout reclamation, and Manager
	// shutdown. Active turns may still be finalizing at this point.
	OnSessionClosing(session *Session)

	// OnSessionClosed fires exactly once after closing completed and any
	// active turn has been shut down and awaited. err is the close error,
	// or nil on a clean close.
	OnSessionClosed(session *Session, err error)

	// OnTurnFinished fires when a turn reaches its terminal state.
	// result and err are the same values Turn.Wait returns.
	// It is called before Turn.Wait unblocks, so observers MUST NOT call
	// Turn.Wait from this callback.
	OnTurnFinished(session *Session, turn *Turn, result *agent.Result, err error)
}

// BaseSessionObserver provides no-op default implementations of every
// SessionObserver method.
type BaseSessionObserver struct{}

// OnSessionStarted is a no-op.
func (BaseSessionObserver) OnSessionStarted(*Session, *Turn) {}

// OnSessionClosing is a no-op.
func (BaseSessionObserver) OnSessionClosing(*Session) {}

// OnSessionClosed is a no-op.
func (BaseSessionObserver) OnSessionClosed(*Session, error) {}

// OnTurnFinished is a no-op.
func (BaseSessionObserver) OnTurnFinished(*Session, *Turn, *agent.Result, error) {}
