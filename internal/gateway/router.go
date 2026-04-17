package gateway

import (
	"context"
	"net/http"
	"sync"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// ChannelRouter routes inbound webhook requests to the correct Agent
// based on per-agent ChannelBinding configurations.
type ChannelRouter struct {
	store model.Store

	mu       sync.RWMutex
	bindings []resolvedBinding
	cancel   context.CancelFunc
}

type resolvedBinding struct {
	agentID string
	binding model.ChannelBinding
	channel Channel
}

// NewChannelRouter creates a new router backed by the given store.
func NewChannelRouter(store model.Store) *ChannelRouter {
	return &ChannelRouter{store: store}
}

// Resolve finds the target Agent and Channel for an inbound webhook request.
// It filters bindings by channelType, then attempts signature verification
// against each candidate. The first binding whose VerifySignature succeeds wins.
//
// Contract: Channel.VerifySignature MUST restore r.Body after reading it
// so that subsequent attempts and ParseRequest can re-read the body.
func (cr *ChannelRouter) Resolve(channelType string, r *http.Request) (string, Channel, error) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()

	for _, b := range cr.bindings {
		if b.binding.Type != channelType {
			continue
		}
		if err := b.channel.VerifySignature(r); err == nil {
			return b.agentID, b.channel, nil
		}
	}
	return "", nil, errdefs.NotFoundf("channel binding %s not found", channelType)
}

// Reload loads all Agent channel configurations from the store and rebuilds
// the routing table. It stops any background goroutines (e.g. Feishu token
// refresh) from the previous generation before starting new ones.
func (cr *ChannelRouter) Reload(ctx context.Context) error {
	agents, _, err := cr.store.ListAgents(ctx, model.ListOptions{Limit: model.MaxPageLimit})
	if err != nil {
		return err
	}

	cr.mu.Lock()
	if cr.cancel != nil {
		cr.cancel()
	}
	cr.mu.Unlock()

	bgCtx, cancel := context.WithCancel(ctx)

	var bindings []resolvedBinding
	for _, a := range agents {
		for _, cb := range a.Config.Channels {
			ch, buildErr := BuildChannel(cb)
			if buildErr != nil {
				telemetry.Warn(ctx, "gateway: skip invalid channel binding",
					otellog.String("agent_id", a.AgentID),
					otellog.String("channel_type", cb.Type),
					otellog.String("error", buildErr.Error()))
				continue
			}
			if fc, ok := ch.(*FeishuChannel); ok {
				fc.StartTokenRefresh(bgCtx)
			}
			bindings = append(bindings, resolvedBinding{
				agentID: a.AgentID,
				binding: cb,
				channel: ch,
			})
		}
	}

	cr.mu.Lock()
	cr.bindings = bindings
	cr.cancel = cancel
	cr.mu.Unlock()
	return nil
}

// GetAgentChannel returns a cached Channel instance for the given agent and
// channel type. Used by NotificationRouter to obtain outbound channels.
func (cr *ChannelRouter) GetAgentChannel(agentID, channelType string) (Channel, bool) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()

	for _, b := range cr.bindings {
		if b.agentID == agentID && b.binding.Type == channelType {
			return b.channel, true
		}
	}
	return nil, false
}
