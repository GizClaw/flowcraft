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
	"github.com/GizClaw/flowcraft/core/telemetry"
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
	// ShutdownTimeout bounds how long Close waits for in-flight requests
	// to finish; <=0 uses 5s.
	ShutdownTimeout time.Duration
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
	// closeErr is what the shutdown returned. The flag above is set
	// before the listener goes away on purpose — Start must not be able
	// to listen again while a shutdown is in flight — so the outcome is
	// recorded here instead: a second Close reports the failure the
	// first one saw rather than a fresh nil.
	closeErr error
	// closeDone is closed once that outcome is recorded: with the flag
	// set before the shutdown, the outcome above is not there yet for a
	// second Close that arrives while the first one is still waiting,
	// and reporting a fresh nil would call a shutdown that has not
	// finished done.
	closeDone chan struct{}
	audit     func(AuditEvent)
	limit     int
	shutdown  time.Duration
	rates     map[Token]*rateWindow
	rateMu    sync.Mutex
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
		rates:       make(map[Token]*rateWindow),
		closeDone:   make(chan struct{}),
	}
	if server.limit <= 0 {
		server.limit = 20
	}
	server.shutdown = opts.ShutdownTimeout
	if server.shutdown <= 0 {
		server.shutdown = 5 * time.Second
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
		// A clean shutdown is ErrServerClosed and is not worth a line;
		// anything else means the endpoint stopped serving on its own,
		// which the host has to be able to see.
		if err := httpServer.Serve(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			telemetry.WarnErr(context.Background(),
				"hostmcp: server stopped serving", err)
		}
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

// Forget drops the rate-limit window of one token. The window is keyed
// by the token's digest, and the caller that retires a token — the host
// that minted it, when a process is replaced or stopped — is the one
// that knows it will not call again: without this the table keeps a
// window for every process the host ever ran.
func (s *Server) Forget(token Token) {
	s.rateMu.Lock()
	delete(s.rates, token)
	s.rateMu.Unlock()
}

// Close stops the listener.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		// The close in flight is what stops the listener. Reporting
		// before it has run is the one answer this must not give: the
		// endpoint may still be serving, and the outcome is not decided
		// yet. This call waits for it and reports the same one.
		done := s.closeDone
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	httpServer := s.http
	s.http = nil
	s.listen = nil
	s.mu.Unlock()
	// The windows belong to tokens this endpoint was serving: a closed
	// server serves none of them again.
	s.rateMu.Lock()
	s.rates = make(map[Token]*rateWindow)
	s.rateMu.Unlock()
	if httpServer == nil {
		s.finishClose(nil)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.shutdown)
	defer cancel()
	err := httpServer.Shutdown(ctx)
	s.finishClose(err)
	return err
}

// finishClose records the shutdown outcome and releases the callers
// waiting on it (see [Server.Close]).
func (s *Server) finishClose(err error) {
	s.mu.Lock()
	s.closeErr = err
	done := s.closeDone
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
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
	identity, minted, ok := s.tokens.Lookup(token)
	if !ok {
		http.Error(writer, "invalid bearer token", http.StatusUnauthorized)
		return
	}
	if !s.allow(minted) {
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

// allow implements a fixed one-second window per token. The window is
// keyed by the token's digest ([Token]): the rate table is not a place
// a replayable secret is allowed to accumulate.
func (s *Server) allow(token Token) bool {
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

// Cached reports whether this server is holding the SDK server it built
// for one plugin. The tools that server exposes are chosen when it is
// built, from the grants of the process that first connected — so the
// host drops it whenever that process is replaced, and a reader that
// reports or verifies a plugin's state can see that happen. A plugin
// that never called the endpoint is not cached.
func (s *Server) Cached(pluginID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servers[pluginID] != nil
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
