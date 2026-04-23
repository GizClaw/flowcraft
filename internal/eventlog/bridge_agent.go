package eventlog

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// agentBridgeMeter / agentBridgeDroppedTotal track AgentStreamBridge
// silent-drops (delta arrived for an actor with no claimed card, or the
// payload didn't decode). Without a counter these would have been
// invisible regressions; per Phase 9 anti-pattern audit the bridge must
// surface them.
var (
	agentBridgeMeter          = telemetry.MeterWithSuffix("eventlog.agent_stream_bridge")
	agentBridgeDroppedTotal   metric.Int64Counter
	agentBridgePublishedTotal metric.Int64Counter
)

func init() {
	var err error
	agentBridgeDroppedTotal, err = agentBridgeMeter.Int64Counter(
		"dropped.total",
		metric.WithDescription("agent stream events dropped before publishing as envelopes"),
	)
	if err != nil {
		slog.Error("agent stream bridge: register dropped counter", "err", err)
	}
	agentBridgePublishedTotal, err = agentBridgeMeter.Int64Counter(
		"published.total",
		metric.WithDescription("agent stream events successfully published as envelopes"),
	)
	if err != nil {
		slog.Error("agent stream bridge: register published counter", "err", err)
	}
}

// AgentStreamBridge translates the in-memory sdk/event stream events
// (stream.delta with embedded type=token / tool_call / tool_result) emitted
// by AgentRuntime into eventlog envelopes — agent.stream.delta /
// agent.tool.invoked / agent.tool.returned. Without this bridge the
// envelope-side TransportHub never sees agent token streams and the
// frontend EnvelopeClient stalls (see Track-B in the cleanup audit).
//
// Lifecycle mirrors KanbanBridge:
//
//	NewAgentStreamBridge(log) -> Attach(ctx, board) -> Close()
//
// The bridge subscribes to board.Bus() (each runtime owns one Board) and is
// constructed only by internal/bootstrap so the events-bridge-lint Makefile
// target can audit a single attach point.
type AgentStreamBridge struct {
	log *SQLiteLog

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	deltaSeq atomic.Int64
}

// NewAgentStreamBridge constructs a bridge bound to log.
func NewAgentStreamBridge(log *SQLiteLog) *AgentStreamBridge {
	return &AgentStreamBridge{log: log}
}

// Attach subscribes to board.Bus() for the agent stream / tool events. It
// derives card_id from the event's ActorID by querying the board for the
// claimed card belonging to that agent; when no claimed card matches, the
// event is dropped because envelopes require a partition.
//
// Calling Attach more than once on the same bridge returns an error.
func (b *AgentStreamBridge) Attach(parent context.Context, board *kanban.Board) error {
	if board == nil {
		return errors.New("agent stream bridge: board is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return errors.New("agent stream bridge: already attached")
	}
	ctx, cancel := context.WithCancel(parent)
	bus := board.Bus()
	sub, err := bus.Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{event.EventStreamDelta},
	})
	if err != nil {
		cancel()
		return err
	}
	b.cancel = cancel
	b.done = make(chan struct{})
	go b.run(ctx, sub, board)
	return nil
}

// Close stops the worker and waits for it to drain.
func (b *AgentStreamBridge) Close() error {
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

func (b *AgentStreamBridge) run(ctx context.Context, sub event.Subscription, board *kanban.Board) {
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
			b.handle(ctx, ev, board)
		}
	}
}

func (b *AgentStreamBridge) handle(ctx context.Context, ev event.Event, board *kanban.Board) {
	delta, ok := ev.Payload.(map[string]any)
	if !ok || delta == nil {
		b.recordDrop(ctx, "payload_not_map", ev.ActorID, "")
		return
	}
	cardID := lookupClaimedCard(board, ev.ActorID)
	if cardID == "" {
		// No claimed card to attribute the delta to; skip — envelopes
		// require a partition and there's no card_id to anchor on.
		// Surfaced via dropped.total{reason=no_claimed_card} +
		// Warn log so this silent path stays observable (Phase 9).
		typeStr, _ := delta["type"].(string)
		b.recordDrop(ctx, "no_claimed_card", ev.ActorID, typeStr)
		return
	}
	switch delta["type"] {
	case "token":
		chunk, _ := delta["content"].(string)
		role, _ := delta["role"].(string)
		seq := b.deltaSeq.Add(1)
		if _, err := PublishAgentStreamDelta(ctx, b.log, cardID, AgentStreamDeltaPayload{
			CardID:   cardID,
			RunID:    ev.RunID,
			DeltaSeq: seq,
			Delta:    chunk,
			Role:     role,
		}, WithTraceIDs(ev.TraceID, ev.SpanID)); err != nil {
			slog.Debug("agent stream bridge: append delta", "err", err)
			b.recordDrop(ctx, "publish_failed", ev.ActorID, "token")
			return
		}
		b.recordPublish(ctx, "token")
	case "tool_call":
		callID, _ := delta["id"].(string)
		toolName, _ := delta["name"].(string)
		arguments, _ := delta["arguments"].(string)
		if _, err := PublishAgentToolInvoked(ctx, b.log, cardID, AgentToolInvokedPayload{
			CardID:    cardID,
			RunID:     ev.RunID,
			ToolName:  toolName,
			CallID:    callID,
			Arguments: arguments,
		}, WithTraceIDs(ev.TraceID, ev.SpanID)); err != nil {
			slog.Debug("agent stream bridge: append tool.invoked", "err", err)
			b.recordDrop(ctx, "publish_failed", ev.ActorID, "tool_call")
			return
		}
		b.recordPublish(ctx, "tool_call")
	case "tool_result":
		callID, _ := delta["tool_call_id"].(string)
		toolName, _ := delta["name"].(string)
		output, _ := delta["content"].(string)
		isErr, _ := delta["is_error"].(bool)
		status := "success"
		errMsg := ""
		if isErr {
			status = "error"
			errMsg = output
		}
		if _, err := PublishAgentToolReturned(ctx, b.log, cardID, AgentToolReturnedPayload{
			CardID:   cardID,
			RunID:    ev.RunID,
			ToolName: toolName,
			CallID:   callID,
			Status:   status,
			Output:   output,
			Error:    errMsg,
		}, WithTraceIDs(ev.TraceID, ev.SpanID)); err != nil {
			slog.Debug("agent stream bridge: append tool.returned", "err", err)
			b.recordDrop(ctx, "publish_failed", ev.ActorID, "tool_result")
			return
		}
		b.recordPublish(ctx, "tool_result")
	default:
		typeStr, _ := delta["type"].(string)
		b.recordDrop(ctx, "unknown_type", ev.ActorID, typeStr)
	}
}

// recordDrop bumps the dropped counter (bounded reason label) and logs
// at Warn so the silent-drop path stays observable.
func (b *AgentStreamBridge) recordDrop(ctx context.Context, reason, actorID, deltaType string) {
	if agentBridgeDroppedTotal != nil {
		agentBridgeDroppedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", reason),
			attribute.String("delta_type", deltaType),
		))
	}
	slog.Warn("agent stream bridge: drop",
		"reason", reason,
		"actor_id", actorID,
		"delta_type", deltaType,
	)
}

func (b *AgentStreamBridge) recordPublish(ctx context.Context, deltaType string) {
	if agentBridgePublishedTotal != nil {
		agentBridgePublishedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("delta_type", deltaType),
		))
	}
}

// lookupClaimedCard finds the card currently claimed by agentID. Empty
// string means "no claimed card" — the bridge drops the delta in that
// case because envelopes require a partition.
func lookupClaimedCard(board *kanban.Board, agentID string) string {
	if board == nil || agentID == "" {
		return ""
	}
	cards := board.Query(kanban.CardFilter{Consumer: agentID, Status: kanban.CardClaimed})
	if len(cards) == 0 {
		return ""
	}
	return cards[0].ID
}
