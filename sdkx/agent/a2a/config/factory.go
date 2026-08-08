package config

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"

	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	a2av0 "github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"google.golang.org/grpc"
)

// Kind is the engine registry key for A2A remote-proxy engines.
const Kind = "a2a"

// Option configures a Factory.
type Option func(*Factory)

// WithHTTPClient injects the HTTP client used by the JSON-RPC transports
// and the agent-card resolver. Defaults to the package's hardened client.
func WithHTTPClient(client *http.Client) Option {
	return func(f *Factory) {
		if client != nil {
			f.httpClient = client
		}
	}
}

// WithGRPCDialOptions sets the dial options for the gRPC transports.
func WithGRPCDialOptions(opts ...grpc.DialOption) Option {
	return func(f *Factory) {
		f.grpcDialOptions = append([]grpc.DialOption(nil), opts...)
	}
}

// Factory builds independent A2A remote-proxy engines.
type Factory struct {
	httpClient      *http.Client
	grpcDialOptions []grpc.DialOption
}

// NewFactory returns an A2A engine factory.
func NewFactory(opts ...Option) *Factory {
	f := &Factory{}
	for _, opt := range opts {
		if opt != nil {
			opt(f)
		}
	}
	return f
}

// Spec implements config.Factory. The kind has a single implementation, so
// Impl is empty and the engine is registered under (Kind, "").
func (*Factory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: Kind}
}

// Capabilities reports the engine kind's claimed optional features. It is
// not part of the config protocol — deploy tooling probes it when present.
func (*Factory) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsResume:  true,
		EmitsCheckpoint: true,
		EmitsUserPrompt: true,
	}
}

// New implements config.Factory: settings are decoded strictly and the
// agent card is resolved (inline, discovered, or synthesised from url)
// before the engine is assembled.
func (f *Factory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	parsed, err := sdkconfig.DecodeSettings[settings](in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("a2a: decode settings: %v", err)
	}
	card, err := f.resolveCard(ctx, parsed)
	if err != nil {
		return nil, err
	}
	headers, err := parsed.headers()
	if err != nil {
		return nil, err
	}
	pollInterval, err := parseDuration(parsed.PollInterval, time.Second)
	if err != nil {
		return nil, err
	}
	transports, err := parseTransports(parsed.PreferredTransports)
	if err != nil {
		return nil, err
	}

	var engineOpts []a2a.Option
	if f.httpClient != nil {
		engineOpts = append(engineOpts, a2a.WithHTTPClient(f.httpClient))
	}
	if len(f.grpcDialOptions) > 0 {
		engineOpts = append(engineOpts, a2a.WithGRPCDialOptions(f.grpcDialOptions...))
	}
	if len(headers) > 0 {
		engineOpts = append(engineOpts, a2a.WithHeaders(headers))
	}
	if parsed.Stream != "" {
		engineOpts = append(engineOpts, a2a.WithStreamMode(parsed.Stream))
	}
	engineOpts = append(engineOpts, a2a.WithPollInterval(pollInterval))
	if parsed.HistoryLength != nil {
		engineOpts = append(engineOpts, a2a.WithHistoryLength(*parsed.HistoryLength))
	}
	if len(parsed.AcceptedOutputModes) > 0 {
		engineOpts = append(engineOpts, a2a.WithAcceptedOutputModes(parsed.AcceptedOutputModes...))
	}
	if len(transports) > 0 {
		engineOpts = append(engineOpts, a2a.WithPreferredTransports(transports...))
	}
	return a2a.New(ctx, card, engineOpts...)
}

// settings is the strictly-decoded engine settings subtree.
type settings struct {
	// URL is an explicit JSON-RPC endpoint; it synthesises an AgentCard.
	URL string `json:"url,omitempty"`
	// CardURL is a base URL whose /.well-known/agent-card.json is fetched.
	CardURL string `json:"card_url,omitempty"`
	// Card is an inline AgentCard.
	Card json.RawMessage `json:"card,omitempty"`
	// Auth configures a static Authorization header.
	Auth *authSettings `json:"auth,omitempty"`
	// Headers are extra static headers attached to every protocol call.
	Headers map[string]string `json:"headers,omitempty"`
	// Stream selects the execution path: auto|on|off.
	Stream string `json:"stream,omitempty"`
	// Protocol pins the protocol version for the URL path: auto|0.3|1.0.
	Protocol string `json:"protocol,omitempty"`
	// PollInterval is the tasks/get polling cadence (Go duration string).
	PollInterval string `json:"poll_interval,omitempty"`
	// HistoryLength limits remote history carried by task responses.
	HistoryLength *int `json:"history_length,omitempty"`
	// AcceptedOutputModes constrains remote output modalities.
	AcceptedOutputModes []string `json:"accepted_output_modes,omitempty"`
	// PreferredTransports orders transport selection (jsonrpc|grpc|http+json).
	PreferredTransports []string `json:"preferred_transports,omitempty"`
}

// authSettings configures a static Authorization header.
type authSettings struct {
	// Scheme is bearer (default), basic, or custom.
	Scheme   string `json:"scheme,omitempty"`
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Header   string `json:"header,omitempty"`
	Value    string `json:"value,omitempty"`
}

// headers merges the auth header with the explicit headers map (explicit
// headers win on key conflicts).
func (s settings) headers() (map[string]string, error) {
	var out map[string]string
	if s.Auth != nil {
		h, err := s.Auth.header()
		if err != nil {
			return nil, err
		}
		out = h
	}
	if len(s.Headers) > 0 {
		if out == nil {
			out = make(map[string]string, len(s.Headers))
		}
		maps.Copy(out, s.Headers)
	}
	return out, nil
}

func (a *authSettings) header() (map[string]string, error) {
	switch strings.ToLower(a.Scheme) {
	case "", "bearer":
		if a.Token == "" {
			return nil, errdefs.Validationf("a2a: auth.scheme bearer requires auth.token")
		}
		return map[string]string{"Authorization": "Bearer " + a.Token}, nil
	case "basic":
		if a.Username == "" || a.Password == "" {
			return nil, errdefs.Validationf("a2a: auth.scheme basic requires auth.username and auth.password")
		}
		cred := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
		return map[string]string{"Authorization": "Basic " + cred}, nil
	case "custom":
		if a.Header == "" || a.Value == "" {
			return nil, errdefs.Validationf("a2a: auth.scheme custom requires auth.header and auth.value")
		}
		return map[string]string{a.Header: a.Value}, nil
	default:
		return nil, errdefs.Validationf("a2a: unsupported auth.scheme %q (want bearer, basic, or custom)", a.Scheme)
	}
}

// resolveCard produces the AgentCard for the engine: inline card, discovered
// card, or a synthesised JSON-RPC card from the explicit URL.
func (f *Factory) resolveCard(ctx context.Context, s settings) (*a2aprotocol.AgentCard, error) {
	switch {
	case len(s.Card) > 0:
		var card a2aprotocol.AgentCard
		if err := json.Unmarshal(s.Card, &card); err != nil {
			return nil, errdefs.Validationf("a2a: inline card: %v", err)
		}
		if len(card.SupportedInterfaces) == 0 {
			return nil, errdefs.Validationf("a2a: inline card declares no supported interfaces")
		}
		return &card, nil
	case s.CardURL != "":
		resolver := agentcard.NewResolver(f.httpClientOrDefault())
		card, err := resolver.Resolve(ctx, s.CardURL)
		if err != nil {
			return nil, err
		}
		return card, nil
	case s.URL != "":
		version := a2aprotocol.Version
		switch s.Protocol {
		case "", "auto", "1.0":
		case "0.3":
			version = a2av0.Version
		default:
			return nil, errdefs.Validationf("a2a: unsupported protocol %q (want auto, 0.3, or 1.0)", s.Protocol)
		}
		return &a2aprotocol.AgentCard{
			SupportedInterfaces: []*a2aprotocol.AgentInterface{{
				URL:             s.URL,
				ProtocolBinding: a2aprotocol.TransportProtocolJSONRPC,
				ProtocolVersion: version,
			}},
		}, nil
	default:
		return nil, errdefs.Validationf("a2a: one of url, card_url, or card is required")
	}
}

func (f *Factory) httpClientOrDefault() *http.Client {
	if f.httpClient != nil {
		return f.httpClient
	}
	return nil
}

func parseDuration(raw string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errdefs.Validationf("a2a: invalid duration %q: %v", raw, err)
	}
	if d <= 0 {
		return 0, errdefs.Validationf("a2a: duration %q must be positive", raw)
	}
	return d, nil
}

func parseTransports(raw []string) ([]a2aprotocol.TransportProtocol, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]a2aprotocol.TransportProtocol, 0, len(raw))
	for _, r := range raw {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "jsonrpc":
			out = append(out, a2aprotocol.TransportProtocolJSONRPC)
		case "grpc":
			out = append(out, a2aprotocol.TransportProtocolGRPC)
		case "http+json", "rest":
			out = append(out, a2aprotocol.TransportProtocolHTTPJSON)
		default:
			return nil, errdefs.Validationf("a2a: unsupported preferred transport %q (want jsonrpc, grpc, or http+json)", r)
		}
	}
	return out, nil
}

var _ sdkconfig.Factory = (*Factory)(nil)
