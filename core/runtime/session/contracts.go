package session

import (
	"context"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
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

type Visibility string

const (
	VisibilityRaw       Visibility = ""
	VisibilityConfirmed Visibility = "confirmed"
)

type Authority string

const (
	AuthorityObserver      Authority = ""
	AuthorityAuthoritative Authority = "authoritative"
)

type AckMode string

const (
	// AckOnDelivery acknowledges a confirmed delivery only after Sink.OnDelta
	// returns nil. If the turn is interrupted while the callback is still
	// running, that delivery is outside the frozen acknowledged prefix.
	AckOnDelivery AckMode = ""
	// AckExplicit requires the authoritative sink to call Turn.Ack. The sink
	// may acknowledge the cursor currently offered to its OnDelta callback
	// before that callback returns.
	AckExplicit AckMode = "explicit"
)

// DeliveryCursor is a turn-global, contiguous confirmed-delivery position.
type DeliveryCursor uint64

// HeaderDeliveryCursor carries a confirmed delivery cursor in an envelope.
const HeaderDeliveryCursor = "session_delivery_cursor"

// DeliveryCursorFromEnvelope reads the confirmed delivery cursor.
func DeliveryCursorFromEnvelope(env event.Envelope) (DeliveryCursor, error) {
	raw := env.Header(HeaderDeliveryCursor)
	if raw == "" {
		return 0, errdefs.Validationf("runtime session: delivery cursor header is missing")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, errdefs.Validationf("runtime session: invalid delivery cursor %q", raw)
	}
	return DeliveryCursor(value), nil
}

// SinkSpec describes one independently buffered stream attachment.
type SinkSpec struct {
	ID string
	// Sink receives stream deltas for this attachment. A Sink may be
	// shared across sessions: session inheritance (see StreamPolicy)
	// attaches the caller's sink to delegated subagent turns as well,
	// so implementations must be safe for concurrent OnDelta calls.
	Sink      agent.StreamSink
	QueueSize int
	// DeliveryTimeout bounds each Sink.OnDelta call. Zero uses the 30-second
	// runtime default; a sink that exceeds the deadline is detached.
	DeliveryTimeout time.Duration
	OnDetach        func(error)
	Visibility      Visibility
	Authority       Authority
	// AckMode controls when confirmed authoritative deliveries become part of
	// the committable prefix. See AckOnDelivery and AckExplicit.
	AckMode    AckMode
	MaxUnacked int
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
	if s.Visibility != VisibilityRaw && s.Visibility != VisibilityConfirmed {
		return errdefs.Validationf("runtime session: invalid SinkSpec.Visibility %q", s.Visibility)
	}
	if s.Authority != AuthorityObserver && s.Authority != AuthorityAuthoritative {
		return errdefs.Validationf("runtime session: invalid SinkSpec.Authority %q", s.Authority)
	}
	if s.AckMode != AckOnDelivery && s.AckMode != AckExplicit {
		return errdefs.Validationf("runtime session: invalid SinkSpec.AckMode %q", s.AckMode)
	}
	if s.MaxUnacked < 0 {
		return errdefs.Validationf("runtime session: SinkSpec.MaxUnacked must not be negative")
	}
	if s.Authority == AuthorityAuthoritative && s.Visibility != VisibilityConfirmed {
		return errdefs.Validationf("runtime session: authoritative sink must be confirmed")
	}
	if s.MaxUnacked > 0 && s.AckMode != AckExplicit {
		return errdefs.Validationf(
			"runtime session: MaxUnacked requires AckExplicit (AckOnDelivery has no unacknowledged window)")
	}
	if s.AckMode == AckExplicit &&
		(s.Visibility != VisibilityConfirmed || s.Authority != AuthorityAuthoritative) {
		return errdefs.Validationf(
			"runtime session: explicit acknowledgements and MaxUnacked require a confirmed authoritative sink")
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
