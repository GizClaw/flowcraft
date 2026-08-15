package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	sdktool "github.com/GizClaw/flowcraft/core/tool"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	otellog "go.opentelemetry.io/otel/log"
)

// DefaultPrefixSeparator joins a server name to a tool name when
// namespacing is enabled.
const DefaultPrefixSeparator = "__"

// DefaultConnectTimeout bounds every connection attempt, initial and
// retried. The build context may carry no deadline at all, so without
// a per-attempt bound a hung stdio child or a black-holed dial would
// stall startup or a background retry forever.
const DefaultConnectTimeout = 30 * time.Second

// DefaultRetryBackoff is the delay before the first background
// reconnection attempt. The delay doubles on each failure up to
// DefaultRetryMaxBackoff.
const (
	DefaultRetryBackoff    = time.Second
	DefaultRetryMaxBackoff = 30 * time.Second
)

// DefaultLivenessInterval is how often a connected server is pinged to
// detect that it died. The go-sdk does not surface a peer disconnect
// until a request fails and, on modern protocol versions, disables its
// own keepalive, so this package probes with the standard MCP ping.
const DefaultLivenessInterval = 15 * time.Second

// Source is a tool.Source that connects MCP servers and exposes their
// tools as ordinary core/tool.Tool values.
//
// Connection is best-effort. AddServer attempts to connect and
// discover tools immediately; a failure that is the server's fault —
// unreachable, missing binary, timeout — schedules background
// reconnection with exponential backoff instead of failing the host,
// and the tools are published to the attached [sdktool.Registrar]
// when the server comes up. A failure that is our configuration's
// fault (validation, rejection) still returns an error to the caller.
// Once connected, a server that dies is reconnected the same way: its
// tools stay visible, and calls fail with per-server NotAvailable
// until the connection is restored.
type Source struct {
	mu        sync.Mutex
	servers   map[string]*server
	retrying  map[string]struct{}
	closed    bool
	registrar sdktool.Registrar

	// baseCtx is the Source-owned context governing background work.
	// It is canceled on Close so retries never outlive the source.
	// The attach context is deliberately NOT used for retries: it is
	// typically request-scoped and gone by the time a retry needs to
	// run.
	baseCtx context.Context
	cancel  context.CancelFunc

	connectTimeout time.Duration
	retryInitial   time.Duration
	retryMax       time.Duration
	liveness       time.Duration
}

// SourceOption configures a Source.
type SourceOption func(*Source)

// WithConnectTimeout bounds each connection attempt. Non-positive
// values fall back to DefaultConnectTimeout.
func WithConnectTimeout(d time.Duration) SourceOption {
	return func(s *Source) {
		if d > 0 {
			s.connectTimeout = d
		}
	}
}

// WithRetryBackoff sets the initial and maximum delays between
// background connection attempts. Non-positive values fall back to
// the defaults, and max is clamped to be at least initial.
func WithRetryBackoff(initial, max time.Duration) SourceOption {
	return func(s *Source) {
		if initial > 0 {
			s.retryInitial = initial
		}
		if max > 0 {
			s.retryMax = max
		}
	}
}

// WithLivenessInterval sets how often a connected server is pinged to
// detect a dead connection. Non-positive values fall back to
// DefaultLivenessInterval.
func WithLivenessInterval(d time.Duration) SourceOption {
	return func(s *Source) {
		if d > 0 {
			s.liveness = d
		}
	}
}

// NewSource returns an empty MCP Source.
func NewSource(opts ...SourceOption) *Source {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Source{
		servers:        make(map[string]*server),
		retrying:       make(map[string]struct{}),
		baseCtx:        ctx,
		cancel:         cancel,
		connectTimeout: DefaultConnectTimeout,
		retryInitial:   DefaultRetryBackoff,
		retryMax:       DefaultRetryMaxBackoff,
		liveness:       DefaultLivenessInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.retryMax < s.retryInitial {
		s.retryMax = s.retryInitial
	}
	return s
}

// Tools implements tool.Source: every discovered tool plus optional
// resource-bridge tools, for every server attached so far.
func (s *Source) Tools() []sdktool.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sdktool.Tool
	for _, srv := range s.servers {
		out = append(out, srv.tools...)
	}
	return out
}

// LazyTools implements tool.Source. MCP tools are discovered eagerly
// when a connection succeeds; a not-yet-connected server contributes
// nothing until the background retry brings it up.
func (s *Source) LazyTools() []sdktool.LazyTool { return nil }

// Attach implements tool.RegistryAttacher. The registrar receives
// every runtime tool publication. Connected servers' current
// projections are published immediately — duplicates of the
// construction-time snapshot are ignored — so a server that connected
// between the registry snapshot and this call is not lost.
func (s *Source) Attach(r sdktool.Registrar) {
	if r == nil {
		return
	}
	var current []sdktool.Tool
	s.mu.Lock()
	s.registrar = r
	for _, srv := range s.servers {
		srv.mu.Lock()
		current = append(current, srv.tools...)
		srv.mu.Unlock()
	}
	s.mu.Unlock()
	for _, t := range current {
		if err := r.Add(t); err != nil && !errdefs.IsConflict(err) {
			telemetry.WarnErr(s.baseCtx, "mcp: publish tool failed", err,
				otellog.String("tool", t.Definition().Name))
		}
	}
}

// server is one live or connecting server plus its exposed tools.
type server struct {
	name      string
	prefix    string
	transport mcpsdk.Transport
	cfg       *serverConfig

	clientName string
	clientVer  string
	clientOpts *mcpsdk.ClientOptions
	onListErr  func(server string, err error)
	resources  bool

	mu      sync.Mutex
	session *mcpsdk.ClientSession
	tools   []sdktool.Tool
}

// currentSession returns the live session or a typed error if the
// server is not connected.
func (s *server) currentSession() (*mcpsdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil, errdefs.NotAvailablef("mcp: server %q is not connected", s.name)
	}
	return s.session, nil
}

// qualify maps a server-side tool name to the registry key.
func (s *server) qualify(name string) string {
	if s.prefix == "" {
		return name
	}
	return s.prefix + name
}

// ServerOption configures one attached server.
type ServerOption func(*serverConfig)

type serverConfig struct {
	prefix      string
	prefixSet   bool
	clientName  string
	clientVer   string
	clientOpts  *mcpsdk.ClientOptions
	onListError func(server string, err error)
	resources   bool
}

// WithPrefix overrides the namespace prefix applied to the server's tool
// names. The default is "<serverName>__".
func WithPrefix(prefix string) ServerOption {
	return func(c *serverConfig) {
		c.prefix = prefix
		c.prefixSet = true
	}
}

// WithClientInfo sets the client identity reported to the server.
func WithClientInfo(name, version string) ServerOption {
	return func(c *serverConfig) {
		c.clientName = name
		c.clientVer = version
	}
}

// WithClientOptions supplies go-sdk client options verbatim.
func WithClientOptions(opts *mcpsdk.ClientOptions) ServerOption {
	return func(c *serverConfig) { c.clientOpts = opts }
}

// WithListErrorHandler installs a callback for tools/list failures that
// happen outside a caller's control.
func WithListErrorHandler(fn func(server string, err error)) ServerOption {
	return func(c *serverConfig) { c.onListError = fn }
}

// WithResources bridges the server's MCP resources into two registry
// tools — <prefix>list_resources and <prefix>read_resource.
func WithResources(enabled bool) ServerOption {
	return func(c *serverConfig) { c.resources = enabled }
}

// AddServer attaches one MCP server. It attempts to connect and
// discover tools immediately; on a server-side failure (unreachable,
// missing binary, timeout) the attempt is retried in the background
// with backoff and AddServer returns nil, so a host can finish
// starting up while the server converges. Validation failures — a
// rejected connection, an invalid request, a duplicate name — are
// configuration errors and are returned to the caller.
//
// The transport must tolerate being connected more than once when a
// background retry is needed; the Stdio and StreamableHTTP transports
// built by this package both do.
func (s *Source) AddServer(
	ctx context.Context,
	name string,
	transport mcpsdk.Transport,
	opts ...ServerOption,
) error {
	if strings.TrimSpace(name) == "" {
		return errdefs.Validationf("mcp: server name is empty")
	}
	if transport == nil {
		return errdefs.Validationf("mcp: server %q: transport is nil", name)
	}

	cfg := &serverConfig{
		prefix:     name + DefaultPrefixSeparator,
		clientName: "flowcraft",
		clientVer:  "v1",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	srv := &server{
		name:       name,
		prefix:     cfg.prefix,
		transport:  transport,
		cfg:        cfg,
		clientName: cfg.clientName,
		clientVer:  cfg.clientVer,
		clientOpts: cfg.clientOpts,
		onListErr:  cfg.onListError,
		resources:  cfg.resources,
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if _, exists := s.servers[name]; exists {
		s.mu.Unlock()
		return errdefs.Validationf("mcp: server %q is already attached", name)
	}
	if _, pending := s.retrying[name]; pending {
		s.mu.Unlock()
		return errdefs.Validationf("mcp: server %q is already attached", name)
	}
	s.mu.Unlock()

	attemptCtx, cancel := context.WithTimeout(ctx, s.connectTimeout)
	session, err := s.connect(attemptCtx, srv, cfg)
	cancel()
	if err != nil {
		if fatalAttachError(ctx, err) {
			return fmt.Errorf("mcp: attach server %q: %w", name, err)
		}
		s.scheduleRetry(srv)
		return nil
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, s.connectTimeout)
	defer cancel()
	if err := s.attachSession(reconcileCtx, srv, session); err != nil {
		if fatalAttachError(ctx, err) {
			return fmt.Errorf("mcp: attach server %q: %w", name, err)
		}
		s.scheduleRetry(srv)
		return nil
	}
	go s.watch(srv)
	return nil
}

// Close stops all background reconnection, closes every session, and
// releases the tool projections. Idempotent.
func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	servers := s.servers
	s.servers = nil
	s.mu.Unlock()

	s.cancel()

	var first error
	for _, srv := range servers {
		srv.mu.Lock()
		session := srv.session
		srv.session = nil
		srv.mu.Unlock()
		if session != nil {
			if err := session.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// connect builds the go-sdk client and performs initialization.
func (s *Source) connect(
	ctx context.Context,
	srv *server,
	cfg *serverConfig,
) (*mcpsdk.ClientSession, error) {
	opts := mcpsdk.ClientOptions{}
	if cfg.clientOpts != nil {
		opts = *cfg.clientOpts
	}
	userHandler := opts.ToolListChangedHandler
	opts.ToolListChangedHandler = func(ctx context.Context, req *mcpsdk.ToolListChangedRequest) {
		if err := s.reconcile(ctx, srv); err != nil && cfg.onListError != nil {
			cfg.onListError(srv.name, err)
		}
		if userHandler != nil {
			userHandler(ctx, req)
		}
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    cfg.clientName,
		Version: cfg.clientVer,
	}, &opts)
	session, err := client.Connect(ctx, srv.transport, nil)
	if err != nil {
		return nil, connectError(srv.name, err)
	}
	return session, nil
}

// attachSession installs a connected session, reconciles the server's
// tool projection, and registers the server on the Source unless it is
// already there (a reconnect). On failure the session is closed and
// srv.session is cleared, so a retry starts from a clean state.
func (s *Source) attachSession(
	ctx context.Context,
	srv *server,
	session *mcpsdk.ClientSession,
) error {
	srv.mu.Lock()
	srv.session = session
	srv.mu.Unlock()

	if err := s.reconcile(ctx, srv); err != nil {
		srv.mu.Lock()
		if srv.session == session {
			srv.session = nil
		}
		srv.mu.Unlock()
		_ = session.Close()
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if other, exists := s.servers[srv.name]; exists && other != srv {
		return errdefs.Validationf("mcp: server %q is already attached", srv.name)
	}
	s.servers[srv.name] = srv
	return nil
}

// reconcile re-lists the server's tools and updates its projection,
// publishing additions and removals to the attached registrar. The
// previous projection is kept on failure, so the model never loses
// sight of tools it was told about.
func (s *Source) reconcile(ctx context.Context, srv *server) error {
	session, err := srv.currentSession()
	if err != nil {
		return err
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return errdefs.NotAvailablef("mcp: server %q: list tools: %v", srv.name, err)
	}

	next := make([]sdktool.Tool, 0, len(res.Tools)+2)
	for _, mt := range res.Tools {
		if mt == nil || mt.Name == "" {
			continue
		}
		next = append(next, newAdaptedTool(srv, srv.qualify(mt.Name), mt))
	}
	if srv.resources {
		for _, spec := range resourceToolSpecs(srv) {
			next = append(next, spec.tool)
		}
	}

	srv.mu.Lock()
	added, removed := diffTools(srv.tools, next)
	srv.tools = next
	srv.mu.Unlock()

	s.publish(added, removed)
	return nil
}

// diffTools splits the move from old to next into additions and
// removals by tool name.
func diffTools(old, next []sdktool.Tool) (added, removed []sdktool.Tool) {
	oldNames := make(map[string]struct{}, len(old))
	for _, t := range old {
		oldNames[t.Definition().Name] = struct{}{}
	}
	nextNames := make(map[string]struct{}, len(next))
	for _, t := range next {
		name := t.Definition().Name
		if _, dup := nextNames[name]; dup {
			continue
		}
		nextNames[name] = struct{}{}
		if _, known := oldNames[name]; !known {
			added = append(added, t)
		}
	}
	for _, t := range old {
		if _, still := nextNames[t.Definition().Name]; !still {
			removed = append(removed, t)
		}
	}
	return added, removed
}

// publish pushes tool additions and removals to the attached
// registrar, if any. A duplicate Add is not an error here: the tool
// may already be present from the construction-time snapshot.
func (s *Source) publish(added, removed []sdktool.Tool) {
	s.mu.Lock()
	reg := s.registrar
	s.mu.Unlock()
	if reg == nil {
		return
	}
	for _, t := range added {
		if err := reg.Add(t); err != nil && !errdefs.IsConflict(err) {
			telemetry.WarnErr(s.baseCtx, "mcp: publish tool failed", err,
				otellog.String("tool", t.Definition().Name))
		}
	}
	for _, t := range removed {
		reg.Remove(t.Definition().Name)
	}
}

// fatalAttachError reports whether a failed attach must surface to the
// caller instead of being retried in the background: configuration
// errors (validation/rejection) and a dead or canceled caller context.
func fatalAttachError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsValidation(err) {
		return true
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	return false
}

// scheduleRetry starts a background reconnection loop for srv unless
// one is already running or the source is closed.
func (s *Source) scheduleRetry(srv *server) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, running := s.retrying[srv.name]; running {
		s.mu.Unlock()
		return
	}
	s.retrying[srv.name] = struct{}{}
	s.mu.Unlock()
	go s.retryLoop(srv)
}

// retryLoop reconnects srv with exponential backoff until it succeeds
// or the source closes.
func (s *Source) retryLoop(srv *server) {
	backoff := s.retryInitial
	for {
		select {
		case <-s.baseCtx.Done():
			s.clearRetrying(srv.name)
			return
		case <-time.After(backoff):
		}

		attemptCtx, cancel := context.WithTimeout(s.baseCtx, s.connectTimeout)
		session, err := s.connect(attemptCtx, srv, srv.cfg)
		cancel()
		if err == nil {
			reconcileCtx, cancel := context.WithTimeout(s.baseCtx, s.connectTimeout)
			err = s.attachSession(reconcileCtx, srv, session)
			cancel()
			if err == nil {
				s.clearRetrying(srv.name)
				go s.watch(srv)
				return
			}
			_ = session.Close()
			if errdefs.IsValidation(err) {
				telemetry.Error(s.baseCtx, "mcp: server rejected attach, giving up",
					otellog.String("server", srv.name),
					otellog.String(telemetry.AttrErrorMessage, err.Error()))
				s.clearRetrying(srv.name)
				return
			}
			if s.baseCtx.Err() != nil {
				s.clearRetrying(srv.name)
				return
			}
			telemetry.WarnErr(s.baseCtx, "mcp: server attach failed, will retry", err,
				otellog.String("server", srv.name))
			backoff = s.nextBackoff(backoff)
			continue
		}

		if errdefs.IsValidation(err) {
			// The peer is there but rejects us — retrying cannot fix a
			// configuration problem, so stop instead of churning.
			telemetry.Error(s.baseCtx, "mcp: server rejected connection, giving up",
				otellog.String("server", srv.name),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			s.clearRetrying(srv.name)
			return
		}
		if s.baseCtx.Err() != nil {
			s.clearRetrying(srv.name)
			return
		}
		telemetry.WarnErr(s.baseCtx, "mcp: server connect failed, will retry", err,
			otellog.String("server", srv.name))
		backoff = s.nextBackoff(backoff)
	}
}

// nextBackoff doubles cur up to the configured maximum.
func (s *Source) nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > s.retryMax {
		return s.retryMax
	}
	return next
}

func (s *Source) clearRetrying(name string) {
	s.mu.Lock()
	delete(s.retrying, name)
	s.mu.Unlock()
}

// watch monitors srv's current session with periodic pings and
// schedules a reconnect when the session dies on the server's side.
// The tool projection is kept: calls fail with per-server NotAvailable
// until the reconnection succeeds.
func (s *Source) watch(srv *server) {
	srv.mu.Lock()
	session := srv.session
	srv.mu.Unlock()
	if session == nil {
		return
	}
	ticker := time.NewTicker(s.liveness)
	defer ticker.Stop()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case <-ticker.C:
		}
		srv.mu.Lock()
		cur := srv.session
		srv.mu.Unlock()
		if cur != session {
			return // a reconnect installed a newer session; it has its own watcher
		}

		pingCtx, cancel := context.WithTimeout(s.baseCtx, s.liveness)
		err := session.Ping(pingCtx, nil)
		cancel()
		if err == nil {
			continue
		}
		if s.baseCtx.Err() != nil {
			return
		}
		srv.mu.Lock()
		stillCurrent := srv.session == session
		if stillCurrent {
			srv.session = nil
		}
		srv.mu.Unlock()
		if !stillCurrent {
			return // a newer session replaced the dead one
		}

		telemetry.Warn(s.baseCtx, "mcp: server connection lost, reconnecting",
			otellog.String("server", srv.name),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		s.scheduleRetry(srv)
		return
	}
}

var (
	_ sdktool.Source           = (*Source)(nil)
	_ sdktool.RegistryAttacher = (*Source)(nil)
)
