package bootstrap

import (
	"github.com/GizClaw/flowcraft/internal/api/ssehub"
	"github.com/GizClaw/flowcraft/internal/api/wshub"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// HubBundle holds the ws + sse hubs together; bootstrap returns both so the
// HTTP router can mount /ws and /sse without re-resolving dependencies.
type HubBundle struct {
	WS  *wshub.Hub
	SSE *ssehub.Hub
}

// WireHubs constructs the WebSocket + SSE hubs and starts their heartbeat
// goroutines. Call HubBundle.Stop on shutdown.
func WireHubs(log *eventlog.SQLiteLog, pol policy.Policy) HubBundle {
	wh := wshub.NewHub(log, pol, wshub.DefaultHubConfig)
	sh := ssehub.NewHub(log, pol, ssehub.DefaultHubConfig)
	wh.Start()
	sh.Start()
	return HubBundle{WS: wh, SSE: sh}
}

// Stop tears down both hubs.
func (h HubBundle) Stop() {
	if h.WS != nil {
		h.WS.Stop()
	}
	if h.SSE != nil {
		h.SSE.Stop()
	}
}
