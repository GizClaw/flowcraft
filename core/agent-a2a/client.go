package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/httpkit"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// newClient connects to the peer named by the config.
//
// The transport is httpkit's for one reason: it injects the caller's trace
// context into every outgoing request, so the peer's run joins this run's trace
// instead of starting a second one that mentions the same task id by accident.
// Two of its defaults are turned down for this protocol:
//
//   - retries, because an A2A message is not idempotent: a retried send can run
//     the task twice on the peer, and a retried stream replays events the caller
//     already consumed.
//   - a client timeout, because a streaming task outlives any fixed bound. The
//     run's own context bounds the call — cancel, drain and the delegation
//     timeout all reach it through ctx.
func newClient(ctx context.Context, cfg Config) (*a2aclient.Client, error) {
	if strings.TrimSpace(cfg.Peer) == "" {
		return nil, errdefs.Validationf("a2a: a peer is required")
	}
	base := httpkit.NewRoundTripper(httpkit.WithoutRetry())
	if token := bearerToken(cfg.Auth); token != "" {
		base = bearerTransport{token: token, base: base}
	}
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{
		a2a.NewAgentInterface(cfg.Peer, a2a.TransportProtocolJSONRPC),
	}, a2aclient.WithJSONRPCTransport(&http.Client{Transport: base}))
	if err != nil {
		return nil, errdefs.Validationf("a2a: connect to %s: %w", cfg.Peer, err)
	}
	return client, nil
}

// bearerTransport attaches the bearer token to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// bearerToken reads the deployment's credential. Only bearer is understood: an
// auth block this build cannot honour must not be silently ignored.
func bearerToken(auth json.RawMessage) string {
	if len(auth) == 0 {
		return ""
	}
	var header struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(auth, &header); err != nil {
		return ""
	}
	if header.Type != "" && header.Type != "bearer" {
		return ""
	}
	return header.Token
}
