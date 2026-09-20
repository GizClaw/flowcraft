package session

import "github.com/GizClaw/flowcraft/core/errdefs"

var (
	ErrSessionClosed   = errdefs.NotAvailablef("runtime session: session is closed")
	ErrPromptUnknown   = errdefs.NotFoundf("runtime session: prompt is unknown")
	ErrPromptDuplicate = errdefs.Conflictf("runtime session: prompt was already replied")
	ErrPromptClosed    = errdefs.NotAvailablef("runtime session: prompt is closed")
	ErrSinkQueueFull   = errdefs.BudgetExceededf("runtime session: sink queue is full")
	// ErrSteerClosed is returned by Turn.Steer once the turn reached a
	// terminal state: nothing will drain the queue after that point.
	ErrSteerClosed = errdefs.NotAvailablef("runtime session: turn is not accepting steer")
	// ErrSteerQueueFull is returned by Turn.Steer when the turn-owned
	// steer queue already holds maxSteerQueue messages. The caller keeps
	// its message and decides whether to retry, queue for the next turn,
	// or surface the rejection.
	ErrSteerQueueFull = errdefs.BudgetExceededf("runtime session: steer queue is full")
	// ErrSteerTooLarge is returned by Turn.Steer for a single message
	// whose JSON form exceeds maxSteerMessageBytes.
	ErrSteerTooLarge = errdefs.BudgetExceededf("runtime session: steer message exceeds the size limit")
)
