package a2a

import (
	"context"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/goccy/go-json"
)

// newClient connects to the peer named by the config.
//
// The transport is a thin one on purpose. It injects the caller's trace context into every
// outgoing request, so the peer's run joins this run's trace instead of starting a second one
// that happens to mention the same task id — and it does nothing else a client library would:
//
//   - no retries, because an A2A message is not idempotent: a retried send can run the task
//     twice on the peer, and a retried stream replays events the caller already consumed;
//   - no client timeout, because a streaming task outlives any fixed bound. The run's own
//     context bounds the call, so cancel and the deployment's timeout both reach it through ctx.
func newClient(ctx context.Context, cfg Config) (*a2aclient.Client, error) {
	if strings.TrimSpace(cfg.Peer) == "" {
		return nil, errdefs.Validationf("a2a: a peer is required")
	}
	var transport http.RoundTripper = traceTransport{}
	if token := bearerToken(cfg.Auth); token != "" {
		transport = bearerTransport{token: token, base: transport}
	}
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{
		a2a.NewAgentInterface(cfg.Peer, a2a.TransportProtocolJSONRPC),
	}, a2aclient.WithJSONRPCTransport(&http.Client{Transport: transport}))
	if err != nil {
		return nil, errdefs.Validationf("a2a: connect to %s: %w", cfg.Peer, err)
	}
	return client, nil
}

// traceTransport renders the caller's span into the outgoing headers, which is the only way a
// remote run can appear as a child of this one.
type traceTransport struct{}

func (traceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	otel.GetTextMapPropagator().Inject(clone.Context(), propagation.HeaderCarrier(clone.Header))
	return http.DefaultTransport.RoundTrip(clone)
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

// bearerToken reads the deployment's credential. Only bearer is understood: an auth block this
// build cannot honour must not be silently ignored, so an unknown type yields no header and the
// peer refuses the call.
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
