package bootstrap

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	projectionwebhook "github.com/GizClaw/flowcraft/internal/projection/webhook"
	senderwebhook "github.com/GizClaw/flowcraft/internal/senders/webhook"
)

// wireWebhookProjectors registers the webhook-domain projectors and the
// outbound sender. The router and the sender share a single SSRF guard so
// the same allow-list policy applies to both inbound dispatch and outbound
// retries.
func wireWebhookProjectors(c *ProjectorComponents, mgr *projection.Manager, log eventlog.Log) error {
	c.WebhookRoutes = projectionwebhook.NewDefaultRouteRegistry()
	ssrf := senderwebhook.NewSSRFGuard()

	c.WebhookRouter = projectionwebhook.NewWebhookRouter(log, c.WebhookRoutes, projectionwebhook.Options{
		SSRF: ssrf,
	})
	if err := mgr.RegisterProjector(c.WebhookRouter, nil); err != nil {
		return fmt.Errorf("register webhook_router projector: %w", err)
	}

	c.WebhookSender = senderwebhook.NewWebhookOutboundSender(log, senderwebhook.Options{
		SSRF: ssrf,
	})
	if err := mgr.RegisterProjector(c.WebhookSender, nil); err != nil {
		return fmt.Errorf("register webhook_outbound_sender projector: %w", err)
	}
	return nil
}
