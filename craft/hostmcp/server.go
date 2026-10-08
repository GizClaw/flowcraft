package hostmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

type identityKey struct{}

func identityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	return identity, ok
}

// ServerOptions configures the host MCP server.
type ServerOptions struct {
	Registry    *Registry
	Tokens      *Tokens
	HostVersion string
	// Audit receives one record per tool call (no payloads).
	Audit func(AuditEvent)
	// RateLimit bounds requests per second per token; <=0 uses 20.
	RateLimit int
}

// AuditEvent describes one primitive call outcome.
type AuditEvent struct {
	PluginID string
	Tool     string
	Outcome  string
	Duration time.Duration
}

// Server exposes the primitive registry over streamable HTTP on
// 127.0.0.1, authenticated by per-plugin bearer tokens. Each plugin id
// gets its own SDK server whose handlers close over the caller's
// identity and grants.
type Server struct {
	registry    *Registry
	tokens      *Tokens
	hostVersion string
	handler     http.Handler

	mu      sync.Mutex
	servers map[string]*mcpsdk.Server
	http    *http.Server
	listen  net.Listener
	url     string
	closed  bool
	audit   func(AuditEvent)
	limit   int
	rates   map[string]*rateWindow
	rateMu  sync.Mutex
}

// NewServer builds the HTTP handler (the listener starts in Start).
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Registry == nil {
		return nil, errdefs.Validationf("hostmcp: Registry is required")
	}
	if opts.Tokens == nil {
		return nil, errdefs.Validationf("hostmcp: Tokens is required")
	}
	server := &Server{
		registry:    opts.Registry,
		tokens:      opts.Tokens,
		hostVersion: opts.HostVersion,
		servers:     make(map[string]*mcpsdk.Server),
		audit:       opts.Audit,
		limit:       opts.RateLimit,
		rates:       make(map[string]*rateWindow),
	}
	if server.limit <= 0 {
		server.limit = 20
	}
	server.handler = mcpsdk.NewStreamableHTTPHandler(
		func(request *http.Request) *mcpsdk.Server {
			identity, ok := identityFromContext(request.Context())
			if !ok {
				return nil
			}
			return server.serverFor(identity)
		},
		&mcpsdk.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
		},
	)
	return server, nil
}

// Start binds 127.0.0.1:0 and serves the handler in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("hostmcp: closed")
	}
	if s.listen != nil {
		return nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errdefs.NotAvailablef("hostmcp: listen: %v", err)
	}
	s.listen = listener
	s.url = "http://" + listener.Addr().String() + "/mcp"
	httpServer := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.http = httpServer
	// The serving goroutine keeps its own reference: Close clears the
	// field, and a field read here would race with it — a scheduling
	// that lost the race then served from a nil server.
	go func() {
		_ = httpServer.Serve(listener)
	}()
	return nil
}

// URL returns the listener endpoint, or "" before Start.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Drop invalidates the cached SDK server of one plugin (grants may have
// changed on reload).
func (s *Server) Drop(pluginID string) {
	s.mu.Lock()
	delete(s.servers, pluginID)
	s.mu.Unlock()
}

// Close stops the listener.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	httpServer := s.http
	s.http = nil
	s.listen = nil
	s.mu.Unlock()
	if httpServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(ctx)
}

// ServeHTTP authenticates the bearer token, enforces loopback-only
// requests, and delegates to the SDK handler.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if err := checkRequest(request); err != nil {
		http.Error(writer, err.Error(), http.StatusForbidden)
		return
	}
	token := bearerToken(request)
	if token == "" {
		http.Error(writer, "missing bearer token", http.StatusUnauthorized)
		return
	}
	identity, ok := s.tokens.Lookup(token)
	if !ok {
		http.Error(writer, "invalid bearer token", http.StatusUnauthorized)
		return
	}
	if !s.allow(token) {
		http.Error(writer, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	ctx := context.WithValue(request.Context(), identityKey{}, identity)
	s.handler.ServeHTTP(writer, request.WithContext(ctx))
}

type rateWindow struct {
	start time.Time
	count int
}

// allow implements a fixed one-second window per token.
func (s *Server) allow(token string) bool {
	now := time.Now()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	window := s.rates[token]
	if window == nil || now.Sub(window.start) >= time.Second {
		window = &rateWindow{start: now}
		s.rates[token] = window
	}
	window.count++
	return window.count <= s.limit
}

func bearerToken(request *http.Request) string {
	header := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

// checkRequest rejects browser-origin requests and non-loopback Host
// headers (DNS rebinding defense).
func checkRequest(request *http.Request) error {
	if request.Header.Get("Origin") != "" {
		return errors.New("origin requests are not allowed")
	}
	host, _, err := net.SplitHostPort(request.Host)
	if err != nil {
		host = request.Host
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errdefs.Forbiddenf("hostmcp: non-loopback host %q", request.Host)
	}
	return nil
}

func (s *Server) serverFor(identity Identity) *mcpsdk.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if server := s.servers[identity.PluginID]; server != nil {
		return server
	}
	server := s.buildServer(identity)
	s.servers[identity.PluginID] = server
	return server
}

func (s *Server) buildServer(identity Identity) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "craft",
		Version: s.hostVersion,
	}, nil)
	for _, tool := range s.registry.Tools() {
		tool := tool
		if tool.Grant != "" && !identity.Grants.Has(tool.Grant) {
			// Keep the tool visible in host_about's denied list, but do
			// not expose it to this caller.
			continue
		}
		mcpsdk.AddTool(server, &mcpsdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		}, func(
			ctx context.Context,
			_ *mcpsdk.CallToolRequest,
			args map[string]any,
		) (*mcpsdk.CallToolResult, any, error) {
			startedAt := time.Now()
			out, err := tool.Handler(ctx, Call{Identity: identity, Params: args})
			if s.audit != nil {
				outcome := "ok"
				if err != nil {
					outcome = "error"
				}
				s.audit(AuditEvent{
					PluginID: identity.PluginID,
					Tool:     tool.Name,
					Outcome:  outcome,
					Duration: time.Since(startedAt),
				})
			}
			if err != nil {
				return nil, nil, err
			}
			payload, err := json.Marshal(out)
			if err != nil {
				return nil, nil, err
			}
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{
					&mcpsdk.TextContent{Text: string(payload)},
				},
			}, nil, nil
		})
	}
	return server
}
