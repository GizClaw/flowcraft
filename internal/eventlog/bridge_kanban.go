package eventlog

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// KanbanBridge translates kanban Board.Bus() task.* events into eventlog
// envelopes and Append()s them inside Atomic transactions.
//
// Lifecycle:
//
//	NewKanbanBridge(log) -> Attach(ctx, board) -> Close()
//
// The bridge subscribes to board.Bus() inside Attach. Close() cancels the
// internal worker goroutine and waits for it to drain. Bridges are private
// to this package; callers must go through eventlog.BootKanbanWithBridge,
// enforced by `make events-bridge-lint`.
type KanbanBridge struct {
	log *SQLiteLog

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewKanbanBridge constructs a bridge bound to log. Attach must be called
// before any task.* events should be persisted.
func NewKanbanBridge(log *SQLiteLog) *KanbanBridge {
	return &KanbanBridge{log: log}
}

// Attach subscribes to board.Bus() and starts the worker goroutine. Calling
// Attach more than once on the same bridge returns an error so callers can't
// accidentally double-publish.
func (b *KanbanBridge) Attach(parent context.Context, board *kanban.Board) error {
	if board == nil {
		return errors.New("kanban bridge: board is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return errors.New("kanban bridge: already attached")
	}
	ctx, cancel := context.WithCancel(parent)
	bus := board.Bus()
	sub, err := bus.Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{
			event.EventType(kanban.EventTaskSubmitted),
			event.EventType(kanban.EventTaskClaimed),
			event.EventType(kanban.EventTaskCompleted),
			event.EventType(kanban.EventTaskFailed),
		},
	})
	if err != nil {
		cancel()
		return err
	}
	b.cancel = cancel
	b.done = make(chan struct{})
	go b.run(ctx, sub)
	return nil
}

// Close stops the worker and waits for it to drain.
func (b *KanbanBridge) Close() error {
	b.mu.Lock()
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	return nil
}

func (b *KanbanBridge) run(ctx context.Context, sub event.Subscription) {
	defer close(b.done)
	defer func() { _ = sub.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			b.handle(ctx, ev)
		}
	}
}

// handle translates a single sdk event into an envelope and appends it.
func (b *KanbanBridge) handle(ctx context.Context, ev event.Event) {
	envType, runtimeID, payload, err := translateKanbanEvent(ev)
	if err != nil {
		slog.Debug("kanban bridge: skip event",
			"type", string(ev.Type), "err", err)
		return
	}

	if _, err := b.log.Atomic(ctx, func(uow UnitOfWork) error {
		return uow.Append(ctx, EnvelopeDraft{
			Partition: PartitionRuntime(runtimeID),
			Type:      envType,
			Version:   1,
			Category:  "business",
			Payload:   payload,
			TraceID:   ev.TraceID,
			SpanID:    ev.SpanID,
		})
	}); err != nil {
		slog.Error("kanban bridge: append failed",
			"type", envType, "runtime_id", runtimeID, "err", err)
	}
}

// taskEnvelopePayload mirrors §7.1.4 with stable JSON keys. The bridge
// translates from the sdk's TaskSubmittedPayload / TaskClaimedPayload /
// TaskCompletedPayload / TaskFailedPayload into this canonical shape so
// the on-wire schema does not depend on the sdk's internal field names.
type taskEnvelopePayload struct {
	CardID        string         `json:"card_id"`
	RuntimeID     string         `json:"runtime_id"`
	TargetAgentID string         `json:"target_agent_id,omitempty"`
	Query         string         `json:"query,omitempty"`
	DispatchNote  string         `json:"dispatch_note,omitempty"`
	Inputs        map[string]any `json:"inputs,omitempty"`
	Producer      string         `json:"producer,omitempty"`
	CronRuleID    string         `json:"cron_rule_id,omitempty"`
	Result        string         `json:"result,omitempty"`
	Error         string         `json:"error,omitempty"`
	ElapsedMs     int64          `json:"elapsed_ms,omitempty"`
}

// translateKanbanEvent maps a kanban sdk event to (envelope_type,
// runtime_id, payload). Returns an error for events the bridge does not
// know how to translate.
func translateKanbanEvent(ev event.Event) (envType, runtimeID string, payload taskEnvelopePayload, err error) {
	switch ev.Type {
	case event.EventType(kanban.EventTaskSubmitted):
		p, ok := ev.Payload.(kanban.TaskSubmittedPayload)
		if !ok {
			return "", "", taskEnvelopePayload{}, errors.New("expected TaskSubmittedPayload")
		}
		return "task.submitted", p.RuntimeID, taskEnvelopePayload{
			CardID:        p.CardID,
			RuntimeID:     p.RuntimeID,
			TargetAgentID: p.TargetAgentID,
			Query:         p.Query,
			Inputs:        p.Inputs,
		}, nil
	case event.EventType(kanban.EventTaskClaimed):
		p, ok := ev.Payload.(kanban.TaskClaimedPayload)
		if !ok {
			return "", "", taskEnvelopePayload{}, errors.New("expected TaskClaimedPayload")
		}
		return "task.claimed", p.RuntimeID, taskEnvelopePayload{
			CardID:        p.CardID,
			RuntimeID:     p.RuntimeID,
			TargetAgentID: p.TargetAgentID,
		}, nil
	case event.EventType(kanban.EventTaskCompleted):
		p, ok := ev.Payload.(kanban.TaskCompletedPayload)
		if !ok {
			return "", "", taskEnvelopePayload{}, errors.New("expected TaskCompletedPayload")
		}
		return "task.completed", p.RuntimeID, taskEnvelopePayload{
			CardID:        p.CardID,
			RuntimeID:     p.RuntimeID,
			TargetAgentID: p.TargetAgentID,
			Result:        p.Output,
			ElapsedMs:     p.ElapsedMs,
		}, nil
	case event.EventType(kanban.EventTaskFailed):
		p, ok := ev.Payload.(kanban.TaskFailedPayload)
		if !ok {
			return "", "", taskEnvelopePayload{}, errors.New("expected TaskFailedPayload")
		}
		return "task.failed", p.RuntimeID, taskEnvelopePayload{
			CardID:        p.CardID,
			RuntimeID:     p.RuntimeID,
			TargetAgentID: p.TargetAgentID,
			Error:         p.Error,
			ElapsedMs:     p.ElapsedMs,
		}, nil
	}
	return "", "", taskEnvelopePayload{}, errors.New("unknown kanban event type")
}
