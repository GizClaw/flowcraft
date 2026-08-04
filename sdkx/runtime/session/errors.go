package session

import "github.com/GizClaw/flowcraft/sdk/errdefs"

var (
	ErrSessionClosed   = errdefs.NotAvailablef("runtime session: session is closed")
	ErrPromptUnknown   = errdefs.NotFoundf("runtime session: prompt is unknown")
	ErrPromptDuplicate = errdefs.Conflictf("runtime session: prompt was already replied")
	ErrPromptClosed    = errdefs.NotAvailablef("runtime session: prompt is closed")
	ErrSinkQueueFull   = errdefs.BudgetExceededf("runtime session: sink queue is full")
)
