// Wires R4 projectors and senders (agent / chat / webhook / audit) onto
// the projection.Manager. Each projector is registered with the optional
// SnapshotStore so chat (RestoreSnapshot) can persist + restore state.
package bootstrap

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projectionagent "github.com/GizClaw/flowcraft/internal/projection/agent"
	"github.com/GizClaw/flowcraft/internal/projection/audit"
	"github.com/GizClaw/flowcraft/internal/projection/chat"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	projectionwebhook "github.com/GizClaw/flowcraft/internal/projection/webhook"
	senderwebhook "github.com/GizClaw/flowcraft/internal/senders/webhook"
)

// R4Components is the set of long-lived R4 instances bootstrap returns so
// callers can plug them into commands / Stop them on shutdown.
type R4Components struct {
	Chat          *chat.ChatProjector
	ChatAutoAck   *chat.ChatAutoAckProjector
	Audit         *audit.AuditProjector
	AgentRun      *projectionagent.AgentRunProjector
	AgentTrace    *projectionagent.AgentTraceProjector
	WebhookRouter *projectionwebhook.WebhookRouter
	WebhookSender *senderwebhook.WebhookOutboundSender
	WebhookRoutes *projectionwebhook.DefaultRouteRegistry
}

// RegisterR4Projectors instantiates and registers the R4 projectors / senders.
// Returns the components struct so the caller can stop the auto_ack and
// webhook sender goroutines on shutdown and inject Chat into authorization.
func RegisterR4Projectors(mgr *projection.Manager, log eventlog.Log, snapshots projection.SnapshotStore) (*R4Components, error) {
	c := &R4Components{}

	c.Audit = audit.NewAuditProjector(log)
	if err := mgr.RegisterProjector(c.Audit, nil); err != nil {
		return nil, fmt.Errorf("register audit projector: %w", err)
	}

	c.AgentRun = projectionagent.NewAgentRunProjector(log)
	if err := mgr.RegisterProjector(c.AgentRun, nil, projection.WithSnapshotStore(snapshots)); err != nil {
		return nil, fmt.Errorf("register agent_run projector: %w", err)
	}
	c.AgentTrace = projectionagent.NewAgentTraceProjector(log)
	if err := mgr.RegisterProjector(c.AgentTrace, nil); err != nil {
		return nil, fmt.Errorf("register agent_trace projector: %w", err)
	}

	c.Chat = chat.NewChatProjector(log)
	if err := mgr.RegisterProjector(c.Chat, nil, projection.WithSnapshotStore(snapshots)); err != nil {
		return nil, fmt.Errorf("register chat projector: %w", err)
	}
	c.ChatAutoAck = chat.NewChatAutoAckProjector(log, c.Chat)
	if err := mgr.RegisterProjector(c.ChatAutoAck, []string{c.Chat.Name()}); err != nil {
		return nil, fmt.Errorf("register chat_auto_ack projector: %w", err)
	}

	c.WebhookRoutes = projectionwebhook.NewDefaultRouteRegistry()
	ssrf := senderwebhook.NewSSRFGuard()
	c.WebhookRouter = projectionwebhook.NewWebhookRouter(log, c.WebhookRoutes, projectionwebhook.Options{
		SSRF: ssrf,
	})
	if err := mgr.RegisterProjector(c.WebhookRouter, nil); err != nil {
		return nil, fmt.Errorf("register webhook_router projector: %w", err)
	}

	c.WebhookSender = senderwebhook.NewWebhookOutboundSender(log, senderwebhook.Options{
		SSRF: ssrf,
	})
	if err := mgr.RegisterProjector(c.WebhookSender, nil); err != nil {
		return nil, fmt.Errorf("register webhook_outbound_sender projector: %w", err)
	}

	return c, nil
}
