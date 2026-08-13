package a2a

import (
	"context"
	"net/http"

	"github.com/GizClaw/flowcraft/core/utils"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2av0 "github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	a2agrpcv0 "github.com/a2aproject/a2a-go/v2/a2agrpc/v0"
	a2agrpcv1 "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"google.golang.org/grpc"
)

// ClientOptions configures how the official A2A client is assembled.
type ClientOptions struct {
	// HTTPClient is used by the JSON-RPC transports. Defaults to
	// utils.NewHttpClient() when nil.
	HTTPClient *http.Client

	// GRPCDialOptions are applied when the gRPC transports establish a
	// connection. Empty uses gRPC defaults (TLS with system roots).
	GRPCDialOptions []grpc.DialOption

	// Headers are attached to every protocol call (authentication tokens,
	// tenant headers, ...). They become ServiceParams on the context and are
	// serialised as HTTP headers by the JSON-RPC transports and as metadata
	// by the gRPC transports.
	Headers map[string]string

	// PreferredTransports overrides the transport-selection priority
	// (JSONRPC / GRPC / HTTP+JSON). Empty follows the remote card's
	// declared interface order.
	PreferredTransports []a2aprotocol.TransportProtocol
}

// newClient assembles an official a2aclient.Client that can reach card for
// every binding this package supports: JSON-RPC over HTTP for both A2A v1.0
// (a2aclient) and v0.3 (a2av0 compat), and gRPC for both versions
// (a2agrpc/v1 and a2agrpc/v0). The factory selects the concrete transport
// from the card's declared interfaces and protocol versions.
func newClient(ctx context.Context, card *a2aprotocol.AgentCard, opts ClientOptions) (*a2aclient.Client, error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = utils.NewHttpClient()
	}
	factoryOpts := []a2aclient.FactoryOption{
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(httpClient),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: httpClient}),
		a2agrpcv1.WithGRPCTransport(opts.GRPCDialOptions...),
		a2agrpcv0.WithGRPCTransport(opts.GRPCDialOptions...),
	}
	if len(opts.PreferredTransports) > 0 {
		factoryOpts = append(factoryOpts, a2aclient.WithConfig(a2aclient.Config{
			PreferredTransports: opts.PreferredTransports,
		}))
	}
	return a2aclient.NewFromCard(ctx, card, factoryOpts...)
}

// rpcCtx returns a context with the configured headers attached as service
// parameters, so every protocol call carries authentication and tenant
// metadata. It is a cheap no-op when no headers are configured.
func (o ClientOptions) rpcCtx(ctx context.Context) context.Context {
	if len(o.Headers) == 0 {
		return ctx
	}
	params := make(a2aclient.ServiceParams, len(o.Headers))
	for k, v := range o.Headers {
		params[k] = []string{v}
	}
	return a2aclient.AttachServiceParams(ctx, params)
}
