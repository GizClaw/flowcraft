// Package webhook contains the WebhookRouter projector that dispatches
// inbound webhook envelopes to downstream URLs configured by RouteRegistry.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// RouterProjectorName is the canonical name for the webhook router projector.
const RouterProjectorName = "webhook_router"

// WebhookRoute describes a single downstream route.
type WebhookRoute struct {
	EndpointID string
	URL        string
	Method     string
	Headers    map[string]string
	Timeout    time.Duration
	MaxRetries int
}

// RouteRegistry maps endpoint IDs to their route configuration.
type RouteRegistry interface {
	Get(endpointID string) (*WebhookRoute, bool)
}

// DefaultRouteRegistry is a simple in-memory registry.
type DefaultRouteRegistry struct {
	mu     sync.RWMutex
	routes map[string]*WebhookRoute
}

// NewDefaultRouteRegistry creates an empty registry.
func NewDefaultRouteRegistry() *DefaultRouteRegistry {
	return &DefaultRouteRegistry{routes: make(map[string]*WebhookRoute)}
}

// Get fetches a route by endpoint id.
func (r *DefaultRouteRegistry) Get(endpointID string) (*WebhookRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[endpointID]
	return route, ok
}

// Register adds or replaces a route.
func (r *DefaultRouteRegistry) Register(route *WebhookRoute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[route.EndpointID] = route
}

// WebhookRouter dispatches inbound webhook events to downstream handlers.
type WebhookRouter struct {
	log        eventlog.Log
	routes     RouteRegistry
	httpClient *http.Client
	ssrf       SSRFGuard
	dlt        projection.DeadLetterSink
}

var _ projection.Projector = (*WebhookRouter)(nil)

// SSRFGuard is a minimal interface implemented by senders.SSRFGuard. The
// router uses the same interface to avoid a circular import; callers wire
// the concrete guard via WithSSRF.
type SSRFGuard interface {
	Check(rawURL string) error
}

// Options bundles router options.
type Options struct {
	HTTPClient *http.Client
	SSRF       SSRFGuard
	DLT        projection.DeadLetterSink
}

// NewWebhookRouter constructs a WebhookRouter.
func NewWebhookRouter(log eventlog.Log, routes RouteRegistry, opts Options) *WebhookRouter {
	r := &WebhookRouter{
		log:        log,
		routes:     routes,
		httpClient: opts.HTTPClient,
		ssrf:       opts.SSRF,
		dlt:        opts.DLT,
	}
	if r.httpClient == nil {
		r.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if r.dlt == nil {
		r.dlt = projection.LogDLT{}
	}
	return r
}

// Name returns the canonical projector name.
func (p *WebhookRouter) Name() string { return RouterProjectorName }

// Subscribes returns the consumed envelope types.
func (p *WebhookRouter) Subscribes() []string {
	return []string{eventlog.EventTypeWebhookInboundReceived}
}

// RestoreMode returns RestoreReplay so deliveries are reattempted on restart.
func (p *WebhookRouter) RestoreMode() projection.RestoreMode { return projection.RestoreReplay }

// OnReady is a no-op for the router.
func (p *WebhookRouter) OnReady(context.Context) error { return nil }

// Apply dispatches the inbound envelope. 5xx errors are returned so the
// runner retries; 4xx errors are written to dead_letters and the envelope
// is acknowledged so the projector advances. SSRF failures are also DLT'd.
func (p *WebhookRouter) Apply(ctx context.Context, _ eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.WebhookInboundBody
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	route, ok := p.routes.Get(payload.EndpointID)
	if !ok {
		// No route configured: not an error. The runner advances the
		// checkpoint; operators can register the route + replay later.
		slog.Debug("webhook router: no route", "endpoint_id", payload.EndpointID)
		return nil
	}
	if p.ssrf != nil {
		if err := p.ssrf.Check(route.URL); err != nil {
			p.deadLetter(ctx, env, "ssrf_blocked", err.Error())
			return nil
		}
	}
	return p.deliver(ctx, route, payload, env)
}

func (p *WebhookRouter) deliver(ctx context.Context, route *WebhookRoute, payload eventlog.WebhookInboundBody, env eventlog.Envelope) error {
	body := payload.Body
	if len(body) > 1<<20 {
		body = body[:1<<20]
	}
	method := route.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, route.URL, bytes.NewReader([]byte(body)))
	if err != nil {
		return fmt.Errorf("webhook router: new request: %w", err)
	}
	if payload.ContentType != "" {
		req.Header.Set("Content-Type", payload.ContentType)
	}
	for k, v := range route.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Webhook-Endpoint-ID", route.EndpointID)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		// Transport: transient — let runner retry.
		return fmt.Errorf("webhook router: transport: %w", err)
	}
	defer resp.Body.Close()
	respSnippet := readSnippet(resp.Body, 1024)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Permanent: ack the event but record DLT for ops.
		p.deadLetter(ctx, env, fmt.Sprintf("http_%d", resp.StatusCode), respSnippet)
		return nil
	default:
		return fmt.Errorf("webhook router: downstream %d", resp.StatusCode)
	}
}

func (p *WebhookRouter) deadLetter(ctx context.Context, env eventlog.Envelope, errorClass, errorMsg string) {
	if err := p.dlt.Write(ctx, projection.DeadLetter{
		ProjectorName: RouterProjectorName,
		Seq:           env.Seq,
		Type:          env.Type,
		Partition:     env.Partition,
		Payload:       env.Payload,
		Err:           fmt.Sprintf("%s: %s", errorClass, errorMsg),
		At:            time.Now().UTC(),
	}); err != nil {
		slog.Error("webhook router: DLT write failed",
			"seq", env.Seq, "endpoint", env.Partition, "err", err)
	}
}

func readSnippet(r io.Reader, n int) string {
	buf, _ := io.ReadAll(io.LimitReader(r, int64(n)))
	return string(buf)
}
