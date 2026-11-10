package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// newClient connects to the peer named by the config.
func newClient(ctx context.Context, cfg Config) (*a2aclient.Client, error) {
	if strings.TrimSpace(cfg.Peer) == "" {
		return nil, errdefs.Validationf("a2a: a peer is required")
	}
	options := make([]a2aclient.FactoryOption, 0, 1)
	if token := bearerToken(cfg.Auth); token != "" {
		options = append(options, a2aclient.WithJSONRPCTransport(&http.Client{
			Transport: bearerTransport{token: token, base: http.DefaultTransport},
		}))
	}
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{
		a2a.NewAgentInterface(cfg.Peer, a2a.TransportProtocolJSONRPC),
	}, options...)
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
