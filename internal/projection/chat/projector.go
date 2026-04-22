// Package chat contains the ChatProjector which maintains chat conversation
// state from event log events.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// ProjectorName is the canonical name used in checkpoint registration.
const ProjectorName = "chat"

// SubscribedEvents lists the event types ChatProjector consumes.
var SubscribedEvents = []string{
	"chat.message.sent",
	"chat.callback.queued",
	"chat.callback.delivered",
	"chat.callback.dismissed",
}

// Conversation tracks the full state of a chat conversation.
type Conversation struct {
	ID               string
	OwnerUserID      string
	Messages         []Message
	PendingCallbacks map[string]*Callback // callbackID → state
	LastMessageAt    time.Time
	MessageCount     int
	DeltaSeq         int64
}

// Message represents a single chat message in a conversation.
type Message struct {
	MessageID string
	Role      string
	Content   string
	SentAt    time.Time
}

// Callback represents a pending callback in a conversation.
type Callback struct {
	CallbackID string
	CardID     string
	Content    string
	QueuedAt   time.Time
}

// ChatProjector maintains chat conversation read-model.
type ChatProjector struct {
	log    eventlog.Log
	mu     sync.RWMutex
	convos map[string]*Conversation // conversationID → state
	byCard map[string]string        // cardID → conversationID
}

var _ projection.Projector = (*ChatProjector)(nil)

// NewChatProjector constructs a ChatProjector.
func NewChatProjector(log eventlog.Log) *ChatProjector {
	return &ChatProjector{
		log:    log,
		convos: make(map[string]*Conversation),
		byCard: make(map[string]string),
	}
}

func (p *ChatProjector) Name() string                        { return ProjectorName }
func (p *ChatProjector) Subscribes() []string                { return SubscribedEvents }
func (p *ChatProjector) RestoreMode() projection.RestoreMode     { return projection.RestoreSnapshot }
func (p *ChatProjector) OnReady(context.Context) error        { return nil }

// SnapshotFormatVersion is bumped whenever the snapshot payload structure changes.
const SnapshotFormatVersion = 1

func (p *ChatProjector) SnapshotFormatVersion() int                            { return SnapshotFormatVersion }
func (p *ChatProjector) SnapshotEvery() (int64, time.Duration)                 { return projection.DefaultSnapshotEveryEvents, projection.DefaultSnapshotEveryPeriod }

// Snapshot returns the current cursor and encoded snapshot payload.
func (p *ChatProjector) Snapshot(ctx context.Context) (int64, []byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	type snapConvo struct {
		ID               string
		OwnerUserID      string
		Messages         []Message
		PendingCallbacks map[string]*Callback
		LastMessageAt   time.Time
		MessageCount    int
		DeltaSeq        int64
	}
	convos := make(map[string]*snapConvo, len(p.convos))
	for id, c := range p.convos {
		convos[id] = &snapConvo{
			ID:               c.ID,
			OwnerUserID:      c.OwnerUserID,
			Messages:         c.Messages,
			PendingCallbacks: c.PendingCallbacks,
			LastMessageAt:    c.LastMessageAt,
			MessageCount:     c.MessageCount,
			DeltaSeq:        c.DeltaSeq,
		}
	}
	cardMap := make(map[string]string, len(p.byCard))
	for k, v := range p.byCard {
		cardMap[k] = v
	}
	data := struct {
		Convos map[string]*snapConvo
		ByCard map[string]string
	}{convos, cardMap}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, nil, fmt.Errorf("chat snapshot: marshal: %w", err)
	}
	cp, err := p.log.Checkpoints().Get(ctx, ProjectorName)
	if err != nil {
		return 0, nil, err
	}
	return cp, payload, nil
}

// LoadSnapshot restores projector state from a previously saved snapshot.
func (p *ChatProjector) LoadSnapshot(_ context.Context, _ int64, payload []byte) error {
	if payload == nil {
		return nil
	}
	type snapConvo struct {
		ID               string
		OwnerUserID      string
		Messages         []Message
		PendingCallbacks map[string]*Callback
		LastMessageAt   time.Time
		MessageCount    int
		DeltaSeq        int64
	}
	data := struct {
		Convos map[string]*snapConvo
		ByCard map[string]string
	}{}
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("chat load snapshot: unmarshal: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convos = make(map[string]*Conversation, len(data.Convos))
	for id, sc := range data.Convos {
		p.convos[id] = &Conversation{
			ID:               sc.ID,
			OwnerUserID:      sc.OwnerUserID,
			Messages:         sc.Messages,
			PendingCallbacks: sc.PendingCallbacks,
			LastMessageAt:    sc.LastMessageAt,
			MessageCount:     sc.MessageCount,
			DeltaSeq:        sc.DeltaSeq,
		}
	}
	p.byCard = make(map[string]string, len(data.ByCard))
	for k, v := range data.ByCard {
		p.byCard[k] = v
	}
	return nil
}

// Apply dispatches to the appropriate handler based on event type.
func (p *ChatProjector) Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	switch env.Type {
	case "chat.message.sent":
		return p.handleMessageSent(ctx, uow, env)
	case "chat.callback.queued":
		return p.handleCallbackQueued(ctx, uow, env)
	case "chat.callback.delivered":
		return p.handleCallbackDelivered(ctx, uow, env)
	case "chat.callback.dismissed":
		return p.handleCallbackDismissed(ctx, uow, env)
	}
	return nil
}

func parseTs(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return t
}

func (p *ChatProjector) handleMessageSent(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.ChatMessageSentPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	convID := payload.ConversationID
	if convID == "" {
		convID = extractConversationID(env.Partition)
	}
	if convID == "" {
		convID = payload.CardID
	}
	conv, ok := p.convos[convID]
	if !ok {
		conv = &Conversation{ID: convID}
		p.convos[convID] = conv
	}
	// First user-authored message claims ownership. Subsequent owners are
	// ignored to avoid takeover races (D.15).
	if conv.OwnerUserID == "" && payload.Role == "user" && env.Actor != nil && env.Actor.Kind == "user" {
		conv.OwnerUserID = env.Actor.ID
	}
	conv.Messages = append(conv.Messages, Message{
		MessageID: payload.MessageID,
		Role:      payload.Role,
		Content:   payload.Content,
		SentAt:    parseTs(env.Ts),
	})
	conv.LastMessageAt = parseTs(env.Ts)
	conv.MessageCount++
	conv.DeltaSeq++
	p.byCard[payload.CardID] = convID
	return nil
}

func (p *ChatProjector) handleCallbackQueued(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.ChatCallbackQueuedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	convID := payload.ConversationID
	if convID == "" {
		convID = extractConversationID(env.Partition)
	}
	if convID == "" {
		convID = payload.CardID
	}
	conv, ok := p.convos[convID]
	if !ok {
		conv = &Conversation{ID: convID, PendingCallbacks: make(map[string]*Callback)}
		p.convos[convID] = conv
	}
	if conv.PendingCallbacks == nil {
		conv.PendingCallbacks = make(map[string]*Callback)
	}
	conv.PendingCallbacks[payload.CallbackID] = &Callback{
		CallbackID: payload.CallbackID,
		CardID:     payload.CardID,
		Content:    payload.Content,
		QueuedAt:   parseTs(env.Ts),
	}
	conv.DeltaSeq++
	p.byCard[payload.CardID] = convID
	return nil
}

func (p *ChatProjector) handleCallbackDelivered(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.ChatCallbackDeliveredPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	convID := payload.ConversationID
	if convID == "" {
		convID = extractConversationID(env.Partition)
	}
	if convID == "" {
		convID = payload.CardID
	}
	conv, ok := p.convos[convID]
	if !ok {
		return nil
	}
	delete(conv.PendingCallbacks, payload.CallbackID)
	conv.DeltaSeq++
	return nil
}

func (p *ChatProjector) handleCallbackDismissed(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.ChatCallbackDismissedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	convID := payload.ConversationID
	if convID == "" {
		convID = extractConversationID(env.Partition)
	}
	if convID == "" {
		convID = payload.CardID
	}
	conv, ok := p.convos[convID]
	if !ok {
		return nil
	}
	delete(conv.PendingCallbacks, payload.CallbackID)
	conv.DeltaSeq++
	return nil
}

func extractConversationID(partition string) string {
	// partition format: "card:<id>"
	if len(partition) > 5 && partition[:5] == "card:" {
		return partition[5:]
	}
	return ""
}

// OwnsCallback checks whether actor owns the callback in the conversation.
func (p *ChatProjector) OwnsCallback(_ context.Context, actor eventlog.Actor, conversationID, callbackID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	conv, ok := p.convos[conversationID]
	if !ok {
		return false
	}
	cb, ok := conv.PendingCallbacks[callbackID]
	if !ok {
		return false
	}
	_, cardConvOk := p.byCard[cb.CardID]
	return cardConvOk && p.convOwnerID(conv) == actor.ID
}

func (p *ChatProjector) convOwnerID(conv *Conversation) string {
	return conv.OwnerUserID
}

// GetConversation returns the conversation by ID (nil if not found).
func (p *ChatProjector) GetConversation(id string) *Conversation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.convos[id]
}

// GetConversationByCard returns the conversation ID for a given card.
func (p *ChatProjector) GetConversationByCard(cardID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	convID, ok := p.byCard[cardID]
	return convID, ok
}

// ErrCallbackNotFound is returned when a callback is not found.
var ErrCallbackNotFound = errors.New("callback not found")
