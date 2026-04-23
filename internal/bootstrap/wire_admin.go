package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	senderwebhook "github.com/GizClaw/flowcraft/internal/senders/webhook"
)

// projectionStatusAdapter adapts projection.Manager into the api package
// interface, copying the snapshot one struct at a time so api doesn't import
// projection.
type projectionStatusAdapter struct{ mgr *projection.Manager }

func newProjectionStatusAdapter(mgr *projection.Manager) api.ProjectionStatusProbe {
	if mgr == nil {
		return nil
	}
	return &projectionStatusAdapter{mgr: mgr}
}

func (a *projectionStatusAdapter) Status() []api.ProjectorStatusSnapshot {
	src := a.mgr.Status()
	out := make([]api.ProjectorStatusSnapshot, 0, len(src))
	for _, s := range src {
		out = append(out, api.ProjectorStatusSnapshot{
			Name:                s.Name,
			CheckpointSeq:       s.CheckpointSeq,
			LatestSeq:           s.LatestSeq,
			Lag:                 s.Lag,
			Ready:               s.Ready,
			ConsecutiveFailures: s.ConsecutiveFailures,
			LastError:           s.LastError,
		})
	}
	return out
}

// projectionReplayAdapter delegates to projection.Manager.ReplayEvent.
type projectionReplayAdapter struct{ mgr *projection.Manager }

func newProjectionReplayAdapter(mgr *projection.Manager) api.ProjectionReplayer {
	if mgr == nil {
		return nil
	}
	return &projectionReplayAdapter{mgr: mgr}
}

func (a *projectionReplayAdapter) ReplayEvent(ctx context.Context, log eventlog.Log, projectorName string, env eventlog.Envelope) error {
	return a.mgr.ReplayEvent(ctx, log, projectorName, env)
}

// webhookReplayAdapter delegates to WebhookOutboundSender.ReplayDelivery.
type webhookReplayAdapter struct {
	sender *senderwebhook.WebhookOutboundSender
}

func newWebhookReplayAdapter(sender *senderwebhook.WebhookOutboundSender) api.WebhookReplayer {
	if sender == nil {
		return nil
	}
	return &webhookReplayAdapter{sender: sender}
}

func (a *webhookReplayAdapter) ReplayDelivery(ctx context.Context, deliveryID string) error {
	return a.sender.ReplayDelivery(ctx, deliveryID)
}

// projectorWebhookSender extracts the WebhookOutboundSender from
// ProjectorComponents, returning nil safely when bootstrap failed before the
// webhook stage.
func projectorWebhookSender(p *ProjectorComponents) *senderwebhook.WebhookOutboundSender {
	if p == nil {
		return nil
	}
	return p.WebhookSender
}
