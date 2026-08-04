package session

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// AskUserFunc is the turn-scoped user-prompt callback supplied to a Host.
type AskUserFunc func(context.Context, agent.UserPrompt) (agent.UserReply, error)

// HostRequest contains the per-turn capabilities needed to construct a Host.
type HostRequest struct {
	Key        Key
	RunID      string
	Interrupts <-chan agent.Interrupt
	AskUser    AskUserFunc
}

// Validate checks the required host-construction contract.
func (r HostRequest) Validate() error {
	if err := r.Key.Validate(); err != nil {
		return err
	}
	if r.RunID == "" {
		return errdefs.Validationf("runtime session: HostRequest.RunID is required")
	}
	if r.Interrupts == nil {
		return errdefs.Validationf("runtime session: HostRequest.Interrupts is required")
	}
	if isNil(r.AskUser) {
		return errdefs.Validationf("runtime session: HostRequest.AskUser is required")
	}
	return nil
}

// SinkSpec describes one independently buffered stream attachment.
type SinkSpec struct {
	ID        string
	Sink      agent.StreamSink
	QueueSize int
	// DeliveryTimeout bounds each Sink.OnDelta call. Zero uses the 30-second
	// runtime default; a sink that exceeds the deadline is detached.
	DeliveryTimeout time.Duration
	OnDetach        func(error)
}

// Validate checks a sink before it is attached to a turn.
func (s SinkSpec) Validate() error {
	if s.ID == "" {
		return errdefs.Validationf("runtime session: SinkSpec.ID is required")
	}
	if isNil(s.Sink) {
		return errdefs.Validationf("runtime session: SinkSpec.Sink is required")
	}
	if s.QueueSize < 0 {
		return errdefs.Validationf("runtime session: SinkSpec.QueueSize must not be negative")
	}
	if s.DeliveryTimeout < 0 {
		return errdefs.Validationf("runtime session: SinkSpec.DeliveryTimeout must not be negative")
	}
	return nil
}

// TurnState is the externally observable lifecycle state of a Turn.
type TurnState string

const (
	TurnStarting     TurnState = "starting"
	TurnRunning      TurnState = "running"
	TurnInterrupting TurnState = "interrupting"
	TurnCompleted    TurnState = "completed"
	TurnInterrupted  TurnState = "interrupted"
	TurnCanceled     TurnState = "canceled"
	TurnFailed       TurnState = "failed"
	TurnAborted      TurnState = "aborted"
)

func (s TurnState) isTerminal() bool {
	switch s {
	case TurnCompleted, TurnInterrupted, TurnCanceled, TurnFailed, TurnAborted:
		return true
	default:
		return false
	}
}
