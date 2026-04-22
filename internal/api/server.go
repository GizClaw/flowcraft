package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	auditcmd "github.com/GizClaw/flowcraft/internal/commands/audit"
	"github.com/GizClaw/flowcraft/internal/errcode"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// ServerConfig holds API server configuration.
type ServerConfig struct {
	Host                     string   `json:"host"`
	Port                     int      `json:"port"`
	RateLimitRPS             float64  `json:"rate_limit_rps"`
	RateLimitBurst           int      `json:"rate_limit_burst"`
	RateLimitBucketExpiry    int      `json:"rate_limit_bucket_expiry"`
	RateLimitCleanupInterval int      `json:"rate_limit_cleanup_interval"`
	MaxBodySize              int64    `json:"max_body_size"`
	CORSOrigins              []string `json:"cors_origins"`
	MaxUploadSize            int64    `json:"max_upload_size"`
	WebDir                   string   `json:"web_dir"`
	WebFS                    fs.FS    `json:"-"`
}

// ServerDeps holds all dependencies for the API server.
type ServerDeps struct {
	Platform           *platform.Platform
	Gateway            GatewayIntegration
	PluginDir          string
	Monitoring         MonitoringConfig
	EventLog           *eventlog.SQLiteLog
	AuditCmds          *auditcmd.Commands
	Policy             policy.Policy
	ProjectionStatus   ProjectionStatusProbe
	ProjectionReplayer ProjectionReplayer
	WebhookReplayer    WebhookReplayer
}

// ProjectionStatusProbe is the read side of projection.Manager that
// /api/admin/projection/status uses. Defined here so handler_admin_projection
// doesn't import the projection package (avoids cycle through wiring).
type ProjectionStatusProbe interface {
	Status() []ProjectorStatusSnapshot
}

// ProjectionReplayer drives DLT replays.
type ProjectionReplayer interface {
	ReplayEvent(ctx context.Context, log eventlog.Log, projectorName string, env eventlog.Envelope) error
}

// WebhookReplayer reschedules a previously failed/exhausted webhook delivery.
type WebhookReplayer interface {
	ReplayDelivery(ctx context.Context, deliveryID string) error
}

// ProjectorStatusSnapshot is the projector status snapshot the handler exposes
// (mirrors projection.ProjectorStatus by structure, not by import).
type ProjectorStatusSnapshot struct {
	Name                string
	CheckpointSeq       int64
	LatestSeq           int64
	Lag                 int64
	Ready               bool
	ConsecutiveFailures int
	LastError           string
}

// MonitoringConfig holds monitoring threshold settings.
type MonitoringConfig struct {
	ErrorRateWarn        float64
	ErrorRateDown        float64
	LatencyP95WarnMs     int64
	ConsecutiveBuckets   int
	NoSuccessDownMinutes int
}

// GatewayIntegration is the subset of Gateway needed for route registration.
type GatewayIntegration interface {
	HandleWebhook(w http.ResponseWriter, r *http.Request)
	Router() *gateway.ChannelRouter
}

// Server is the HTTP API server.
type Server struct {
	server *http.Server
	deps   ServerDeps
	config ServerConfig
	done   chan struct{}

	wsTickets *wsTicketStore
	jwt       *JWTConfig
}

const ownerRealmID = "owner"

// NewServer creates and configures the API server with all routes.
func NewServer(cfg ServerConfig, deps ServerDeps, jwtCfg *JWTConfig) *Server {
	s := &Server{
		deps:      deps,
		config:    cfg,
		done:      make(chan struct{}),
		wsTickets: newWSTicketStore(),
		jwt:       jwtCfg,
	}

	h := newOAPIHandler(s)
	ogenSrv, err := oas.NewServer(h, s,
		oas.WithErrorHandler(ogenErrorHandler(s)),
	)
	if err != nil {
		panic("api: ogen server: " + err.Error())
	}

	mux := http.NewServeMux()

	// ogen handles all OpenAPI-defined routes under /api/.
	ogenHTTP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := ContextWithHTTP(r.Context(), w, r)
		ogenSrv.ServeHTTP(w, r.WithContext(ctx))
	})
	mux.Handle("/api/", http.StripPrefix("/api", ogenHTTP))

	// Health check at root level (also defined in OpenAPI under /api/).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// WebSocket is not covered by OpenAPI.
	mux.HandleFunc("GET /api/ws", s.handleWS)

	// Webhook routes are dynamic per channel.
	if s.deps.Gateway != nil {
		mux.HandleFunc("POST /api/webhook/{channel}", s.deps.Gateway.HandleWebhook)
	}

	// Admin audit endpoints. Mounted directly because they read from the
	// projector-maintained audit_entries table and are RBAC-gated to super
	// admins; the OpenAPI schema documents them as observability surface.
	if s.deps.EventLog != nil {
		mux.HandleFunc("GET /api/admin/audit", s.handleAdminAuditList)
		mux.HandleFunc("GET /api/admin/audit/{seq}", s.handleAdminAuditGet)
	}

	// R5 admin endpoints (projection + webhook delivery introspection).
	mux.HandleFunc("GET /api/admin/projection/status", s.handleAdminProjectionStatus)
	if s.deps.EventLog != nil {
		mux.HandleFunc("GET /api/admin/projection/dead-letters", s.handleAdminDeadLettersList)
		mux.HandleFunc("POST /api/admin/projection/dead-letters/{id}/replay", s.handleAdminDeadLetterReplay)
		mux.HandleFunc("GET /api/admin/webhooks/deliveries", s.handleAdminWebhookDeliveries)
		mux.HandleFunc("POST /api/admin/webhooks/deliveries/{id}/replay", s.handleAdminWebhookReplay)
		mux.HandleFunc("GET /api/events/latest-seq", s.handleEventsLatestSeq)
		mux.HandleFunc("GET /api/events/stream", s.handleEventsSSE)
		mux.HandleFunc("GET /api/events/ws", s.handleEventsWS)
	}

	// SPA fallback: method-agnostic "/" is less specific than "/api/" in all
	// dimensions, so no conflict. Only serve SPA for GET requests.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		s.handleSPA(w, r)
	})

	handler := s.middleware(mux)
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	telemetry.Info(ctx, "api: server listening", otellog.String("addr", ln.Addr().String()))
	return s.server.Serve(ln)
}

// Shutdown gracefully shuts down the server and stops background goroutines.
func (s *Server) Shutdown(ctx context.Context) error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) resolveRealmID(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	return ownerRealmID, nil
}

// --- Response helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		telemetry.Error(context.Background(), "api: encode response", otellog.String("error", err.Error()))
	}
}

func writeJSONTo(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	code, status := errcode.Resolve(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": errcode.PublicMessage(err),
		},
	})
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errdefs.Validationf("invalid request body: %v", err)
	}
	return nil
}

func parsePagination(r *http.Request) model.ListOptions {
	opts := model.ListOptions{}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	opts.Cursor = r.URL.Query().Get("cursor")
	return opts
}

func errDepMissing(name string) error {
	return errdefs.NotAvailablef("%s is not configured", name)
}

func (s *Server) resolvedMonitoringConfig() MonitoringConfig {
	cfg := s.deps.Monitoring
	if cfg.ErrorRateWarn <= 0 {
		cfg.ErrorRateWarn = 0.05
	}
	if cfg.ErrorRateDown <= 0 {
		cfg.ErrorRateDown = 0.20
	}
	if cfg.LatencyP95WarnMs <= 0 {
		cfg.LatencyP95WarnMs = 3000
	}
	if cfg.ConsecutiveBuckets <= 0 {
		cfg.ConsecutiveBuckets = 3
	}
	if cfg.NoSuccessDownMinutes <= 0 {
		cfg.NoSuccessDownMinutes = 2
	}
	return cfg
}
