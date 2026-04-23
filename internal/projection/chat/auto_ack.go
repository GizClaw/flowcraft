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

// pendingAck tracks a single status_update callback that is awaiting auto-ack.
type pendingAck struct {
	callbackID string
	cardID     string
	convoID    string
	deadline   time.Time
	seq        int64
}

// ChatAutoAckProjector automatically acks status_update callbacks after a
// timeout. Timers are not driven by time.AfterFunc — instead a single ticker
// goroutine sweeps the pending map and publishes ChatCallbackDelivered for any
// entry whose deadline has elapsed. This keeps the projector compatible with a
// fake clock in tests and avoids leaking goroutines per callback (an
// anti-pattern called out in docs/event-sourcing-plan.md §11.1).
//
// Restart semantics are preserved by RestoreWindow(24h): on cold start the
// projector replays the last 24h of callback events, repopulates pending
// entries, and the ticker resumes sweeping.
type ChatAutoAckProjector struct {
	log      eventlog.Log
	timeout  time.Duration
	chatProj *ChatProjector

	now func() time.Time // injectable clock; defaults to time.Now

	mu      sync.Mutex
	pending map[string]*pendingAck // callbackID → pending entry

	tickInterval time.Duration
	stopOnce     sync.Once
	done         chan struct{}
	wakeup       chan struct{}
	wg           sync.WaitGroup
}

var _ projection.Projector = (*ChatAutoAckProjector)(nil)

// AutoAckTimeout is the default auto-ack timeout for status_update callbacks.
const AutoAckTimeout = 30 * time.Minute

// defaultTickInterval bounds how late an auto-ack can fire after its deadline.
// Small enough to keep latency tight in tests; large enough to be cheap.
const defaultTickInterval = 5 * time.Second

// NewChatAutoAckProjector constructs a ChatAutoAckProjector. Call Start once
// the surrounding ProjectorManager has reached its ready barrier.
func NewChatAutoAckProjector(log eventlog.Log, chatProj *ChatProjector) *ChatAutoAckProjector {
	return &ChatAutoAckProjector{
		log:          log,
		timeout:      AutoAckTimeout,
		chatProj:     chatProj,
		now:          time.Now,
		pending:      make(map[string]*pendingAck),
		tickInterval: defaultTickInterval,
		done:         make(chan struct{}),
		wakeup:       make(chan struct{}, 1),
	}
}

// SetTimeout overrides the auto-ack timeout. Used by tests.
func (p *ChatAutoAckProjector) SetTimeout(d time.Duration) {
	p.mu.Lock()
	p.timeout = d
	p.mu.Unlock()
}

// SetClock overrides the wall clock and tick interval; intended for tests.
func (p *ChatAutoAckProjector) SetClock(now func() time.Time, tick time.Duration) {
	p.mu.Lock()
	if now != nil {
		p.now = now
	}
	if tick > 0 {
		p.tickInterval = tick
	}
	p.mu.Unlock()
}

func (p *ChatAutoAckProjector) Name() string                        { return AutoAckProjectorName }
func (p *ChatAutoAckProjector) RestoreMode() projection.RestoreMode { return projection.RestoreWindow }
func (p *ChatAutoAckProjector) WindowSize() time.Duration           { return 24 * time.Hour }
func (p *ChatAutoAckProjector) Subscribes() []string {
	return []string{"chat.callback.queued", "chat.callback.delivered", "chat.callback.dismissed"}
}

// OnReady starts the sweep goroutine after restore completes. The
// ProjectorManager calls OnReady exactly once after the projector has caught
// up, satisfying the bootstrap order in §2.4.
func (p *ChatAutoAckProjector) OnReady(context.Context) error {
	p.wg.Add(1)
	go p.sweepLoop()
	return nil
}

// AutoAckProjector does not use snapshots.
func (p *ChatAutoAckProjector) SnapshotFormatVersion() int { return 0 }
func (p *ChatAutoAckProjector) SnapshotEvery() (int64, time.Duration) {
	return 0, 0
}
func (p *ChatAutoAckProjector) Snapshot(ctx context.Context) (int64, []byte, error) {
	return 0, nil, nil
}
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
	if _, exists := p.pending[payload.CallbackID]; !exists {
		p.pending[payload.CallbackID] = &pendingAck{
			callbackID: payload.CallbackID,
			cardID:     payload.CardID,
			convoID:    payload.ConversationID,
			deadline:   p.now().Add(p.timeout),
			seq:        env.Seq,
		}
	}
	p.mu.Unlock()
	p.kick()
	return nil
}

func (p *ChatAutoAckProjector) handleCallbackDone(ctx context.Context, env eventlog.Envelope) error {
	var payload struct {
		CallbackID string `json:"callback_id"`
	}
	if json.Unmarshal(env.Payload, &payload) != nil {
		return nil
	}
	p.mu.Lock()
	delete(p.pending, payload.CallbackID)
	p.mu.Unlock()
	return nil
}

// kick wakes the sweep goroutine without blocking. Used after pending entries
// change so a freshly-queued (or freshly-cancelled) callback is reflected in
// the next iteration without waiting a full tick.
func (p *ChatAutoAckProjector) kick() {
	select {
	case p.wakeup <- struct{}{}:
	default:
	}
}

// sweepLoop runs until Stop() is called, periodically firing any auto-ack
// whose deadline has elapsed.
func (p *ChatAutoAckProjector) sweepLoop() {
	defer p.wg.Done()
	t := time.NewTicker(p.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
		case <-p.wakeup:
		}
		p.sweepOnce()
	}
}

// sweepOnce fires every pending entry whose deadline has elapsed. Public so
// tests using a fake clock can trigger a deterministic sweep without touching
// the ticker.
func (p *ChatAutoAckProjector) sweepOnce() {
	p.mu.Lock()
	now := p.now()
	var due []*pendingAck
	for id, e := range p.pending {
		if !now.Before(e.deadline) {
			due = append(due, e)
			delete(p.pending, id)
		}
	}
	p.mu.Unlock()
	for _, e := range due {
		p.publishAutoAck(e)
	}
}

// publishAutoAck emits chat.callback.delivered with a system actor. It is safe
// for the corresponding "real" delivery to win the race: the consumer is
// idempotent on callback_id so the duplicate envelope is harmless.
func (p *ChatAutoAckProjector) publishAutoAck(e *pendingAck) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishChatCallbackDeliveredInTx(ctx, uow, e.cardID, eventlog.ChatCallbackDeliveredPayload{
			CardID:         e.cardID,
			ConversationID: e.convoID,
			CallbackID:     e.callbackID,
			DeliveredTo:    "system_auto",
			Success:        true,
		}, eventlog.WithActor(eventlog.Actor{Kind: "system", ID: "auto_ack"}))
	})
	if err != nil {
		slog.Error("chat auto_ack: publish delivered failed",
			"callback_id", e.callbackID, "card_id", e.cardID, "err", err)
		return
	}
	slog.Info("chat auto_ack: delivered", "callback_id", e.callbackID, "card_id", e.cardID)
}

// Stop signals the sweep goroutine to exit and waits for it to finish.
func (p *ChatAutoAckProjector) Stop() {
	p.stopOnce.Do(func() {
		close(p.done)
	})
	p.wg.Wait()
	p.mu.Lock()
	p.pending = nil
	p.mu.Unlock()
}
