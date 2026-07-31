package kanban

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// Event kinds published on [Kanban.Bus]. One kind per state
// transition; the set is exactly the [Status] values a card can move
// into.
//
// The kind is written to the [HeaderKind] header on every envelope, so
// a subscriber using a coarse pattern such as [PatternAll] can route
// on it without re-parsing the subject.
const (
	EventCardSubmitted = "kanban.card.submitted"
	EventCardClaimed   = "kanban.card.claimed"
	EventCardSuspended = "kanban.card.suspended"
	EventCardDone      = "kanban.card.done"
	EventCardFailed    = "kanban.card.failed"
	EventCardCancelled = "kanban.card.cancelled"
)

// Well-known headers on every envelope this package emits. The board
// scope travels on event.HeaderKanbanScopeID, set via
// [event.Envelope.SetKanbanScopeID].
const (
	// HeaderKind carries the event kind constant.
	HeaderKind = "kanban_kind"
	// HeaderCardID carries the card id.
	HeaderCardID = "card_id"
)

// PayloadVersion is stamped on every emitted payload. Future fields are
// additive, so a consumer that pins version 1 keeps working.
const PayloadVersion = 1

// CardEvent is the payload of every kanban event. One shape for all
// transitions: a subscriber deserialises once and switches on Status,
// rather than maintaining a struct per kind.
type CardEvent struct {
	Version int `json:"version"`

	CardID  string `json:"card_id"`
	ScopeID string `json:"scope_id"`

	// Status is the state the card just entered. It mirrors the event
	// kind, and is the field to switch on.
	Status Status `json:"status"`

	TargetAgentID string `json:"target_agent_id,omitempty"`
	Producer      string `json:"producer,omitempty"`
	Consumer      string `json:"consumer,omitempty"`
	Query         string `json:"query,omitempty"`

	// Output and Error mirror the card's Result, present on terminal
	// transitions.
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`

	// ResumeRef is present on a suspend transition.
	ResumeRef string `json:"resume_ref,omitempty"`

	// ElapsedMs is the time from submission to this transition.
	ElapsedMs int64 `json:"elapsed_ms"`

	Meta map[string]string `json:"meta,omitempty"`
}

// kindFor maps a status to the event kind announcing arrival in it.
func kindFor(s Status) string {
	switch s {
	case StatusPending:
		return EventCardSubmitted
	case StatusClaimed:
		return EventCardClaimed
	case StatusSuspended:
		return EventCardSuspended
	case StatusDone:
		return EventCardDone
	case StatusFailed:
		return EventCardFailed
	case StatusCancelled:
		return EventCardCancelled
	}
	return ""
}

// publish emits the transition snap represents. A publish failure is
// swallowed: an overloaded observer must not roll back a state
// transition that already happened.
//
// A resumed card re-enters StatusPending and therefore re-publishes
// EventCardSubmitted. That is intended — to a consumer the card is
// once again work waiting to be claimed, and ResumeRef distinguishes
// it from a first submission.
func (k *Kanban) publish(ctx context.Context, snap *Card) {
	if k.bus == nil || snap == nil {
		return
	}
	kind := kindFor(snap.Status)
	if kind == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	payload := CardEvent{
		Version:   PayloadVersion,
		CardID:    snap.ID,
		ScopeID:   k.scopeID,
		Status:    snap.Status,
		Producer:  snap.Producer,
		Consumer:  snap.Consumer,
		ResumeRef: snap.ResumeRef,
		ElapsedMs: snap.Elapsed().Milliseconds(),
		Meta:      snap.Meta,
	}
	if snap.Task != nil {
		payload.TargetAgentID = snap.Task.TargetAgentID
		payload.Query = snap.Task.Query
	}
	if snap.Result != nil {
		payload.Output = snap.Result.Output
		payload.Error = snap.Result.Error
	}

	env, err := event.NewEnvelope(ctx, subjectFor(kind, snap.ID), payload)
	if err != nil {
		return
	}
	env.SetHeader(HeaderKind, kind)
	env.SetHeader(HeaderCardID, snap.ID)
	if k.scopeID != "" {
		env.SetKanbanScopeID(k.scopeID)
	}
	_ = k.bus.Publish(ctx, env)
}
