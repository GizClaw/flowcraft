package gateway

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// RecentMessageReader returns the most recent N assistant messages for a
// conversation. After R5 the canonical source is the ChatProjector — the
// router uses this small interface so the gateway package does not need to
// import projection/chat (and so tests can stub the reader cheaply).
type RecentMessageReader interface {
	RecentAssistantMessages(conversationID string, limit int) []string
}

// NotificationRouter subscribes to per-runtime EventBus events and routes
// notifications to configured outbound channels based on Agent.Config.Notification.
type NotificationRouter struct {
	gateway      *Gateway
	store        model.Store
	recentReader RecentMessageReader
}

// NewNotificationRouter creates a notification router. The recentReader is
// optional; when nil, callback summary fall back to the generic event push.
func NewNotificationRouter(gw *Gateway, store model.Store) *NotificationRouter {
	return &NotificationRouter{gateway: gw, store: store}
}

// WithRecentReader wires the projector-backed RecentMessageReader. Bootstrap
// calls this once the ChatProjector is up.
func (nr *NotificationRouter) WithRecentReader(r RecentMessageReader) *NotificationRouter {
	nr.recentReader = r
	return nr
}

// SubscribeSession subscribes to a runtime board's Bus for notification-relevant
// events. The subscription goroutine exits when the bus is closed or ctx is cancelled.
func (nr *NotificationRouter) SubscribeSession(ctx context.Context, sb *kanban.TaskBoard) {
	sub, err := sb.Bus().Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{
			event.EventGraphEnd,
			event.EventNodeError,
			event.EventType(kanban.EventTaskCompleted),
			event.EventType(kanban.EventTaskFailed),
			event.EventType(kanban.EventCallbackDone),
		},
	})
	if err != nil {
		telemetry.Error(ctx, "gateway: notification router subscribe failed",
			otellog.String("runtime_id", sb.ScopeID()),
			otellog.String("error", err.Error()))
		return
	}

	go func() {
		defer func() { _ = sub.Close() }()
		for ev := range sub.Events() {
			nr.handleEvent(ctx, ev)
		}
	}()
}

func (nr *NotificationRouter) handleEvent(ctx context.Context, ev event.Event) {
	agentID := nr.resolveAgentID(ctx, ev)
	if agentID == "" {
		return
	}

	a, err := nr.store.GetAgent(ctx, agentID)
	if err != nil {
		return
	}
	if a.Config.Notification == nil || !a.Config.Notification.Enabled {
		return
	}

	if ev.Type == event.EventType(kanban.EventCallbackDone) {
		nr.pushCallbackSummary(ctx, agentID, a.Config.Notification.ChannelName, ev)
		return
	}

	isFailure := ev.Type == event.EventNodeError || ev.Type == event.EventType(kanban.EventTaskFailed)
	isFinal := ev.Type == event.EventGraphEnd ||
		ev.Type == event.EventType(kanban.EventTaskCompleted)

	cfg := a.Config.Notification
	switch cfg.Granularity {
	case "all":
		nr.push(ctx, agentID, cfg.ChannelName, ev)
	case "final":
		if isFinal {
			nr.push(ctx, agentID, cfg.ChannelName, ev)
		}
	case "failure":
		if isFailure {
			nr.push(ctx, agentID, cfg.ChannelName, ev)
		}
	default:
		if isFinal {
			nr.push(ctx, agentID, cfg.ChannelName, ev)
		}
	}
}

// pushCallbackSummary reads the last assistant message from the conversation
// history (the Dispatcher's summary after processing a callback) and pushes it.
func (nr *NotificationRouter) pushCallbackSummary(ctx context.Context, agentID, channelName string, ev event.Event) {
	if channelName == "" {
		return
	}
	if payload, ok := ev.Payload.(kanban.CallbackDonePayload); ok && payload.Error != "" {
		nr.push(ctx, agentID, channelName, ev)
		return
	}
	ch, ok := nr.gateway.Router().GetAgentChannel(agentID, channelName)
	if !ok {
		return
	}
	outCh, ok := ch.(OutboundChannel)
	if !ok {
		return
	}

	runtimeID := extractRuntimeID(ev.Payload)

	var filters []model.ListFilter
	if runtimeID != "" {
		filters = append(filters, model.WithListRuntimeID(runtimeID))
	}
	convs, _, err := nr.store.ListConversations(ctx, agentID, model.ListOptions{Limit: 5}, filters...)
	if err != nil || len(convs) == 0 {
		nr.push(ctx, agentID, channelName, ev)
		return
	}

	if nr.recentReader == nil {
		nr.push(ctx, agentID, channelName, ev)
		return
	}
	for _, conv := range convs {
		msgs := nr.recentReader.RecentAssistantMessages(conv.ID, 5)
		if len(msgs) == 0 {
			continue
		}
		// Most-recent-last semantics: scan from the end so the latest
		// assistant message wins.
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i] == "" {
				continue
			}
			if err := outCh.Send(ctx, runtimeID, msgs[i]); err != nil {
				telemetry.Warn(ctx, "gateway: callback summary push failed",
					otellog.String("channel", channelName),
					otellog.String("error", err.Error()))
			}
			return
		}
	}

	nr.push(ctx, agentID, channelName, ev)
}

// resolveAgentID extracts the AgentID from event payloads or ActorID.
func (nr *NotificationRouter) resolveAgentID(_ context.Context, ev event.Event) string {
	if agentID := extractAgentID(ev.Payload); agentID != "" {
		return agentID
	}
	if ev.ActorID != "" {
		return ev.ActorID
	}
	return ""
}

func extractRuntimeID(payload any) string {
	switch p := payload.(type) {
	case kanban.CallbackDonePayload:
		return p.RuntimeID
	case kanban.CallbackStartPayload:
		return p.RuntimeID
	case kanban.TaskCompletedPayload:
		return p.RuntimeID
	case kanban.TaskFailedPayload:
		return p.RuntimeID
	case kanban.TaskSubmittedPayload:
		return p.RuntimeID
	case kanban.TaskClaimedPayload:
		return p.RuntimeID
	case map[string]any:
		runtimeID, _ := p["runtime_id"].(string)
		return runtimeID
	default:
		return ""
	}
}

func extractAgentID(payload any) string {
	switch p := payload.(type) {
	case kanban.TaskCompletedPayload:
		return p.TargetAgentID
	case kanban.TaskFailedPayload:
		return p.TargetAgentID
	case kanban.TaskSubmittedPayload:
		return p.TargetAgentID
	case kanban.TaskClaimedPayload:
		return p.TargetAgentID
	case kanban.CallbackDonePayload:
		return p.AgentID
	case kanban.CallbackStartPayload:
		return p.AgentID
	case map[string]any:
		agentID, _ := p["agent_id"].(string)
		if agentID != "" {
			return agentID
		}
		targetAgentID, _ := p["target_agent_id"].(string)
		return targetAgentID
	default:
		return ""
	}
}

func (nr *NotificationRouter) push(ctx context.Context, agentID, channelName string, ev event.Event) {
	if channelName == "" {
		return
	}
	ch, ok := nr.gateway.Router().GetAgentChannel(agentID, channelName)
	if !ok {
		return
	}
	outCh, ok := ch.(OutboundChannel)
	if !ok {
		return
	}

	msg := FormatNotification(ev)
	runtimeID := extractRuntimeID(ev.Payload)
	if err := outCh.Send(ctx, runtimeID, msg); err != nil {
		telemetry.Warn(ctx, "gateway: notification send failed",
			otellog.String("channel", channelName),
			otellog.String("error", err.Error()))
	}
}
