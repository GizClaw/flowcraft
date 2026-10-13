package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
)

// TurnSink durably commits canonical conversation messages. Success means the
// source and its durable derivation work were accepted; asynchronous derivation
// branch failures do not retroactively fail the commit.
type TurnSink interface {
	CommitTurn(context.Context, Turn) error
}

// ObservedAtMetadataKey names the message-level metadata key that anchors a
// message to the time it was observed: an RFC3339 timestamp carried on the
// committed record. Writers stamp it only where that time is actually known --
// a live host when it commits the turn (the arrival time it has), an importer
// from the session time the source conversation carries -- and leave it absent
// otherwise. A reader must not synthesize it from anything else: CreatedAt is
// the storage time of the record, and a bulk import of old history would turn
// it into an "import day" anchor, which is worse than none.
const ObservedAtMetadataKey = "observed_at"

type Turn struct {
	Scope          Scope
	ConversationID string
	IdempotencyKey string
	Messages       []message.Message
	// MessageMetadata optionally tags each message on its own, positionally
	// aligned with Messages. A host importing a conversation from elsewhere
	// keeps the source's own identifiers here (a dataset turn id, for example)
	// so a derived item can be traced back to the imported message by identity
	// instead of being recognized by its text; the observation anchor
	// (ObservedAtMetadataKey) travels the same channel. An empty entry falls
	// back to
	// Metadata, which applies to the whole turn.
	MessageMetadata []Metadata
	Metadata        Metadata
}

func (t Turn) Validate() error {
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(t.ConversationID) == "" {
		return NewError(KindInvalidRequest, "turn", errors.New("memory: conversation_id is required"))
	}
	if strings.TrimSpace(t.IdempotencyKey) == "" {
		return NewError(KindInvalidRequest, "turn", errors.New("memory: idempotency_key is required"))
	}
	if len(t.Messages) == 0 {
		return NewError(KindInvalidRequest, "turn", errors.New("memory: messages are required"))
	}
	if len(t.MessageMetadata) > 0 && len(t.MessageMetadata) != len(t.Messages) {
		return NewError(KindInvalidRequest, "turn", fmt.Errorf(
			"memory: %d message metadata entries for %d messages", len(t.MessageMetadata), len(t.Messages)))
	}
	for index, item := range t.Messages {
		if err := item.Validate(); err != nil {
			return NewError(KindInvalidRequest, "turn", fmt.Errorf("memory: message %d: %w", index, err))
		}
	}
	return nil
}

func (t Turn) Clone() Turn {
	messages := make([]message.Message, len(t.Messages))
	for index, item := range t.Messages {
		messages[index] = item.Clone()
	}
	t.Messages = messages
	messageMetadata := make([]Metadata, len(t.MessageMetadata))
	for index, item := range t.MessageMetadata {
		messageMetadata[index] = item.Clone()
	}
	t.MessageMetadata = messageMetadata
	t.Metadata = t.Metadata.Clone()
	return t
}
