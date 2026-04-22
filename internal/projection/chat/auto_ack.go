// Package chat contains the ChatAutoAckProjector which automatically acknowledges
// status_update callbacks after a timeout, and the ChatProjector for conversation state.
package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// AutoAckProjectorName is the canonical name for the auto-ack projector.
const AutoAckProjectorName = "chat_auto_ack"

// ChatAutoAckProjector automatically acks status_update callbacks after a timeout.
// It uses RestoreWindow mode with a 24h window so timers are rebuilt on restart.
type ChatAutoAckProjector struct {
	log       eventlog.Log
	timeout   time.Duration
	chatProj  *ChatProjector
	mu        sync.RWMutex
	timers    map[string]*time.Timer  // callbackID → timer
	timerSeqs map[string]int64        // callbackID → event seq when timer was set
	convoIDs  map[string]string       // callbackID → conversation_id
}

var _ projection.Projector = (*ChatAutoAckProjector)(nil)

// AutoAckTimeout is the default auto-ack timeout for status_update callbacks.
const AutoAckTimeout = 30 * time.Minute

// NewChatAutoAckProjector constructs a ChatAutoAckProjector.
func NewChatAutoAckProjector(log eventlog.Log, chatProj *ChatProjector) *ChatAutoAckProjector {
	return &ChatAutoAckProjector{
		log:       log,
		timeout:   AutoAckTimeout,
		chatProj:  chatProj,
		timers:    make(map[string]*time.Timer),
		timerSeqs: make(map[string]int64),
		convoIDs:  make(map[string]string),
	}
}

// SetTimeout overrides the auto-ack timeout. Used by tests.
func (p *ChatAutoAckProjector) SetTimeout(d time.Duration) {
	p.mu.Lock()
	p.timeout = d
	p.mu.Unlock()
}

func (p *ChatAutoAckProjector) Name() string    { return AutoAckProjectorName }
func (p *ChatAutoAckProjector) RestoreMode() projection.RestoreMode { return projection.RestoreWindow }
func (p *ChatAutoAckProjector) WindowSize() time.Duration        { return 24 * time.Hour }
func (p *ChatAutoAckProjector) Subscribes() []string {
	return []string{"chat.callback.queued", "chat.callback.delivered", "chat.callback.dismissed"}
}
func (p *ChatAutoAckProjector) OnReady(context.Context) error { return nil }

// AutoAckProjector does not use snapshots.
func (p *ChatAutoAckProjector) SnapshotFormatVersion() int { return 0 }
func (p *ChatAutoAckProjector) SnapshotEvery() (int64, time.Duration) {
	return 0, 0
}
func (p *ChatAutoAckProjector) Snapshot(ctx context.Context) (int64, []byte, error) { return 0, nil, nil }
func (p *ChatAutoAckProjector) LoadSnapshot(context.Context, int64, []byte) error { return nil }

// Apply handles chat.callback.queued for status_update type auto-ack.
func (p *ChatAutoAckProjector) Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	switch env.Type {
	case "chat.callback.queued":
		return p.handleCallbackQueued(ctx, uow, env)
	case "chat.callback.delivered", "chat.callback.dismissed":
		return p.handleCallbackDone(ctx, env)
	}
	return nil
}

func (p *ChatAutoAckProjector) handleCallbackQueued(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.ChatCallbackQueuedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	if payload.ContentType != "status_update" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convoIDs[payload.CallbackID] = payload.ConversationID
	p.startTimer(payload.CallbackID, payload.CardID, env.Seq)
	return nil
}

func (p *ChatAutoAckProjector) startTimer(callbackID, cardID string, seq int64) {
	if _, exists := p.timers[callbackID]; exists {
		return
	}
	timeout := p.timeout
	p.timers[callbackID] = time.AfterFunc(timeout, func() {
		p.doAutoAck(callbackID, cardID)
	})
	p.timerSeqs[callbackID] = seq
}

// doAutoAck publishes chat.callback.delivered with a system actor. It is safe
// to call concurrently with handleCallbackDone: the latter cancels the timer
// before doAutoAck fires; if the publish wins the race, the dedup happens at
// the projector level (the consumer is idempotent on callback_id).
func (p *ChatAutoAckProjector) doAutoAck(callbackID, cardID string) {
	p.mu.Lock()
	convoID := p.convoIDs[callbackID]
	delete(p.timers, callbackID)
	delete(p.timerSeqs, callbackID)
	delete(p.convoIDs, callbackID)
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishChatCallbackDeliveredInTx(ctx, uow, cardID, eventlog.ChatCallbackDeliveredPayload{
			CardID:         cardID,
			ConversationID: convoID,
			CallbackID:     callbackID,
			DeliveredTo:    "system_auto",
			Success:        true,
		}, eventlog.WithActor(eventlog.Actor{Kind: "system", ID: "auto_ack"}))
	})
	if err != nil {
		slog.Error("chat auto_ack: publish delivered failed",
			"callback_id", callbackID, "card_id", cardID, "err", err)
		return
	}
	slog.Info("chat auto_ack: delivered", "callback_id", callbackID, "card_id", cardID)
}

func (p *ChatAutoAckProjector) handleCallbackDone(ctx context.Context, env eventlog.Envelope) error {
	var payload struct {
		CallbackID string `json:"callback_id"`
	}
	if json.Unmarshal(env.Payload, &payload) != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.timers[payload.CallbackID]; ok {
		t.Stop()
		delete(p.timers, payload.CallbackID)
		delete(p.timerSeqs, payload.CallbackID)
		delete(p.convoIDs, payload.CallbackID)
	}
	return nil
}

// Stop cancels all pending timers.
func (p *ChatAutoAckProjector) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.timers {
		t.Stop()
	}
	p.timers = nil
	p.timerSeqs = nil
	p.convoIDs = nil
}
