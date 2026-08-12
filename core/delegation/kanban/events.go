package kanban

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/delegation"
	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/event"
)

const (
	EventCardSubmitted = "delegation.kanban.card.submitted"
	EventCardClaimed   = "delegation.kanban.card.claimed"
	EventCardSuspended = "delegation.kanban.card.suspended"
	EventCardDone      = "delegation.kanban.card.done"
	EventCardFailed    = "delegation.kanban.card.failed"
	EventCardCancelled = "delegation.kanban.card.cancelled"

	HeaderKind          = "kanban_kind"
	HeaderCardID        = "card_id"
	HeaderKanbanScopeID = "kanban_scope_id"

	PayloadVersion = 1
)

// SetKanbanScopeID stores a backend scope through the ordinary event header API.
func SetKanbanScopeID(envelope *event.Envelope, id string) {
	envelope.SetHeader(HeaderKanbanScopeID, id)
}

// KanbanScopeID reads a backend scope through the ordinary event header API.
func KanbanScopeID(envelope event.Envelope) string {
	return envelope.Header(HeaderKanbanScopeID)
}

// CardEvent is the typed payload emitted for each delegation card transition.
type CardEvent struct {
	Version   int                      `json:"version"`
	CardID    string                   `json:"card_id"`
	ScopeID   string                   `json:"scope_id"`
	Status    Status                   `json:"status"`
	Producer  string                   `json:"producer,omitempty"`
	Consumer  string                   `json:"consumer,omitempty"`
	Request   *delegation.AsyncRequest `json:"request,omitempty"`
	Response  *sdkdelegation.Response  `json:"response,omitempty"`
	ResumeRef string                   `json:"resume_ref,omitempty"`
	ElapsedMs int64                    `json:"elapsed_ms"`
	Meta      map[string]string        `json:"meta,omitempty"`
}

func kindFor(status Status) string {
	switch status {
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
	default:
		return ""
	}
}

func (b *Board) publish(ctx context.Context, snapshot *Card) {
	if b.bus == nil || snapshot == nil {
		return
	}
	kind := kindFor(snapshot.Status)
	if kind == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload := CardEvent{
		Version:   PayloadVersion,
		CardID:    snapshot.ID,
		ScopeID:   b.scopeID,
		Status:    snapshot.Status,
		Producer:  snapshot.Producer,
		Consumer:  snapshot.Consumer,
		ResumeRef: snapshot.ResumeRef,
		ElapsedMs: snapshot.Elapsed().Milliseconds(),
		Meta:      cloneMetadata(snapshot.Meta),
	}
	if snapshot.Task != nil {
		request := cloneAsyncRequest(snapshot.Task.Request)
		payload.Request = &request
	}
	if snapshot.Result != nil {
		response := cloneResponse(snapshot.Result.Response)
		payload.Response = &response
	}
	envelope, err := event.NewEnvelope(ctx, subjectFor(kind, snapshot.ID), payload)
	if err != nil {
		return
	}
	envelope.SetHeader(HeaderKind, kind)
	envelope.SetHeader(HeaderCardID, snapshot.ID)
	if b.scopeID != "" {
		SetKanbanScopeID(&envelope, b.scopeID)
	}
	_ = b.bus.Publish(ctx, envelope)
}

const cardSubjectPrefix = "delegation.kanban.card."

func subjectFor(kind, cardID string) event.Subject {
	suffix := kind[len(cardSubjectPrefix):]
	return event.Subject(fmt.Sprintf(
		"%s%s.%s", cardSubjectPrefix, sanitiseID(cardID), suffix))
}

// PatternCard matches every delegation backend event for one card.
func PatternCard(cardID string) event.Pattern {
	return event.Pattern(fmt.Sprintf(
		"%s%s.>", cardSubjectPrefix, sanitiseID(cardID)))
}

// PatternAll matches all delegation kanban backend events.
func PatternAll() event.Pattern {
	return event.Pattern(cardSubjectPrefix + ">")
}

func sanitiseID(id string) string {
	if id == "" {
		return "_"
	}
	out := make([]byte, 0, len(id))
	for index := range len(id) {
		switch id[index] {
		case '.', '*', '>':
			out = append(out, '_')
		default:
			out = append(out, id[index])
		}
	}
	return string(out)
}
