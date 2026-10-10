package hooks

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/core/delegation/kanban"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// observerBuffer bounds the observer's subscription buffer. Card events
// are small and rare next to tool traffic, so the default of 64 is
// generous rather than tight.
const observerBuffer = 64

// Observer is the in-core producer of the subagent events: it forwards
// delegation card transitions — a card claimed, a card terminal — to
// SubagentStart and SubagentStop. The kanban board owns the semantics
// of a delegation starting and stopping, so the adapter ships with this
// package instead of being rewritten by every application.
type Observer struct {
	runner *Manager
	bus    event.Bus

	mu     sync.Mutex
	cancel context.CancelFunc
	sub    event.Subscription
}

// NewObserver builds the adapter over a runner and an event bus.
func NewObserver(runner *Manager, bus event.Bus) (*Observer, error) {
	if runner == nil {
		return nil, errdefs.Validationf("hooks observer: runner is required")
	}
	if bus == nil {
		return nil, errdefs.Validationf("hooks observer: event bus is required")
	}
	return &Observer{runner: runner, bus: bus}, nil
}

// Wire subscribes to the delegation events and starts the forwarding
// loop. It implements [resource.Wireable] and is idempotent.
//
// The loop owns its lifetime: it runs on its own context rather than
// the wiring context, which belongs to the build that produced it, and
// stops in [Observer.Close].
func (o *Observer) Wire(_ context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sub != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := o.bus.Subscribe(
		ctx, kanban.PatternAll(), event.WithBufferSize(observerBuffer))
	if err != nil {
		cancel()
		return errdefs.Validationf("hooks observer: subscribe: %v", err)
	}
	o.cancel, o.sub = cancel, sub
	go o.loop(ctx, sub.C())
	return nil
}

// Close stops the loop and releases the subscription. It is
// idempotent, and safe on an observer that was never wired.
func (o *Observer) Close() error {
	o.mu.Lock()
	cancel, sub := o.cancel, o.sub
	o.cancel, o.sub = nil, nil
	o.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	return sub.Close()
}

// loop forwards card events until the subscription or the loop context
// ends.
func (o *Observer) loop(ctx context.Context, events <-chan event.Envelope) {
	for {
		select {
		case envelope, ok := <-events:
			if !ok {
				return
			}
			var card kanban.CardEvent
			if err := envelope.Decode(&card); err != nil {
				telemetry.Warn(ctx, "hooks observer: decode card event failed",
					attribute.String("subject", string(envelope.Subject)),
					attribute.String("error", err.Error()))
				continue
			}
			name, ok := subagentEvent(card.Status)
			if !ok {
				continue
			}
			o.runner.Fire(ctx, name, subagentPayload(card))
		case <-ctx.Done():
			return
		}
	}
}

// subagentEvent maps a card status onto the subagent hook event: the
// claim is the start, every terminal status is the stop.
func subagentEvent(status kanban.Status) (string, bool) {
	switch status {
	case kanban.StatusClaimed:
		return EventSubagentStart, true
	case kanban.StatusDone, kanban.StatusFailed, kanban.StatusCanceled:
		return EventSubagentStop, true
	default:
		return "", false
	}
}

// subagentPayload is the event object a subagent hook receives: who is
// running, on which card and run, and how it ended. Target and message
// carry the delegated instruction — content, so an untrusted source
// never sees them.
func subagentPayload(card kanban.CardEvent) map[string]any {
	payload := map[string]any{
		"subagent": card.Consumer,
		"card_id":  card.CardID,
		"run_id":   card.RunID,
		"status":   string(card.Status),
	}
	if card.Request != nil {
		payload["target"] = card.Request.Request.Target
		payload["message"] = card.Request.Request.Input
	}
	return payload
}
