// Aggregates the per-domain projector / sender wiring (audit, agent, chat,
// webhook) into a single bootstrap step. Each domain has its own
// wire_<domain>.go that knows how to construct + register its projectors;
// this file only fans out to them and returns the long-lived components so
// callers can plug them into commands and stop them on shutdown.
package bootstrap

import (
	"github.com/GizClaw/flowcraft/internal/eventlog"
	projectionagent "github.com/GizClaw/flowcraft/internal/projection/agent"
	"github.com/GizClaw/flowcraft/internal/projection/audit"
	"github.com/GizClaw/flowcraft/internal/projection/chat"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	projectionwebhook "github.com/GizClaw/flowcraft/internal/projection/webhook"
	senderwebhook "github.com/GizClaw/flowcraft/internal/senders/webhook"
)

// ProjectorComponents bundles the long-lived projector / sender instances
// produced by RegisterDomainProjectors so callers can plug them into
// commands (e.g. ChatProjector → policy) and stop the goroutine-owning
// senders on shutdown.
type ProjectorComponents struct {
	Chat          *chat.ChatProjector
	ChatAutoAck   *chat.ChatAutoAckProjector
	Audit         *audit.AuditProjector
	AgentRun      *projectionagent.AgentRunProjector
	AgentTrace    *projectionagent.AgentTraceProjector
	WebhookRouter *projectionwebhook.WebhookRouter
	WebhookSender *senderwebhook.WebhookOutboundSender
	WebhookRoutes *projectionwebhook.DefaultRouteRegistry
}

// RegisterDomainProjectors instantiates and registers every domain projector
// (audit, agent, chat, webhook) on the shared Manager in a fixed order so
// dependsOn relationships (e.g. chat_auto_ack → chat) resolve cleanly.
//
// The Manager is not started here; the caller owns Start so it can sequence
// projector startup with the rest of the bootstrap (commands, transports).
func RegisterDomainProjectors(mgr *projection.Manager, log eventlog.Log, snapshots projection.SnapshotStore) (*ProjectorComponents, error) {
	c := &ProjectorComponents{}
	if err := wireAuditProjector(c, mgr, log); err != nil {
		return nil, err
	}
	if err := wireAgentProjectors(c, mgr, log, snapshots); err != nil {
		return nil, err
	}
	if err := wireChatProjectors(c, mgr, log, snapshots); err != nil {
		return nil, err
	}
	if err := wireWebhookProjectors(c, mgr, log); err != nil {
		return nil, err
	}
	return c, nil
}
