package mcp

import (
	"context"
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

// DefaultRetryBackoff is the delay after a failed background connect
// attempt before the next one. The first attempt runs immediately; the
// delay doubles on each failure up to DefaultRetryMaxBackoff.
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
// Connection is best-effort and runs entirely in the background.
// AddServer validates and registers a server, schedules its first
// connect attempt, and returns immediately; a failure that is the
// server's fault — unreachable, missing binary, timeout — schedules
// background reconnection with exponential backoff instead of failing
// the host, and the tools are published to the attached
// [sdktool.Registrar] when the server comes up. A failure that is our
// configuration's fault (validation, rejection) is logged and the
// connection is given up; hosts that need a server to be up can await
// it explicitly with [Source.WaitReady]. Once connected, a server that
// dies is reconnected the same way: its tools stay visible, and calls
// fail with per-server NotAvailable until the connection is restored.
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
	servers := make([]*server, 0, len(s.servers))
	for _, srv := range s.servers {
		servers = append(servers, srv)
	}
	s.mu.Unlock()
	var out []sdktool.Tool
	for _, srv := range servers {
		srv.mu.Lock()
		out = append(out, srv.tools...)
		srv.mu.Unlock()
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

	// ready is closed exactly once, on the server's first terminal
	// state: the first successful attach (readyErr nil) or a give-up
	// in the background loop (readyErr set). WaitReady blocks on it.
	// After Close, servers that never connected are woken with a
	// NotAvailable error so waiters do not hang.
	ready    chan struct{}
	readyErr error
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

// readyState returns the server's readiness channel and terminal
// error. A nil channel means the server reached its first terminal
// state: readyErr nil means connected, non-nil means the background
// loop gave up (or the source closed). An open channel means the first
// connection attempt has not finished yet.
func (s *server) readyState() (chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready, s.readyErr
}

// markReady closes the readiness channel exactly once. A nil error
// marks the first successful attach; a non-nil error marks a give-up
// or Close so WaitReady fails fast instead of waiting out its timeout.
func (s *server) markReady(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil {
		return
	}
	s.readyErr = err
	close(s.ready)
	s.ready = nil
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
	required    bool
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

// WithRequired marks a server as required. All servers connect and
// retry the same way either way; the flag is for hosts that cannot
// start without the server, which should mark it required and then
// await [Source.WaitReady] so a background give-up (validation,
// rejection, source closed) surfaces as an error instead of a silent
// missing tool set.
func WithRequired() ServerOption {
	return func(c *serverConfig) { c.required = true }
}

// AddServer attaches one MCP server. It validates the configuration,
// registers the server, schedules the first connection attempt to run
// immediately in the background, and returns nil. No connect work runs
// on the caller's critical path: a hung server stalls a background
// attempt bounded by the per-attempt timeout, never the host.
// Validation errors that used to return synchronously — a rejected
// connection, an invalid request — are now logged and given up in the
// background; only local configuration errors (empty name, nil
// transport, duplicate name) and an already-canceled context are
// returned to the caller. Hosts that require a server to be up should
// call [Source.WaitReady].
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mcp: attach server %q: %w", name, err)
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
		ready:      make(chan struct{}),
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
	s.servers[name] = srv
	s.mu.Unlock()

	s.scheduleRetry(srv)
	return nil
}

// WaitReady blocks until the named server has connected and its tools
// are published, or fails fast when the background loop gives up on it
// (validation/rejection), the source closes, or timeout elapses. A
// non-positive timeout waits only on ctx. Once a server has connected,
// WaitReady returns nil immediately even if the connection later drops
// and reconnects — the host-startup question is "is it up yet", not
// "is it up right now".
func (s *Source) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	s.mu.Lock()
	srv := s.servers[name]
	closed := s.closed
	s.mu.Unlock()
	if srv == nil {
		if closed {
			return errdefs.NotAvailablef("mcp: source is closed")
		}
		return errdefs.Validationf("mcp: server %q is not attached", name)
	}

	ready, err := srv.readyState()
	if ready == nil {
		if err != nil {
			return fmt.Errorf("mcp: server %q: %w", name, err)
		}
		return nil
	}

	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case <-ready:
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return fmt.Errorf("mcp: server %q: %w", name, ctx.Err())
		}
		return errdefs.Timeoutf("mcp: server %q not ready within %s", name, timeout)
	}

	if _, err := srv.readyState(); err != nil {
		return fmt.Errorf("mcp: server %q: %w", name, err)
	}
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
		srv.markReady(errdefs.NotAvailablef("mcp: source is closed"))
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
// already there (a reconnect). On failure — reconcile, a concurrently
// closed Source, or an already-attached name — the session is closed
// and srv.session is cleared, so a retry starts from a clean state and
// no live connection is orphaned.
func (s *Source) attachSession(
	ctx context.Context,
	srv *server,
	session *mcpsdk.ClientSession,
) error {
	srv.mu.Lock()
	srv.session = session
	srv.mu.Unlock()

	if err := s.reconcile(ctx, srv); err != nil {
		abandonAttach(srv, session)
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		abandonAttach(srv, session)
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if other, exists := s.servers[srv.name]; exists && other != srv {
		abandonAttach(srv, session)
		return errdefs.Validationf("mcp: server %q is already attached", srv.name)
	}
	s.servers[srv.name] = srv
	return nil
}

// abandonAttach tears down a session that failed to attach. It clears
// srv.session when it still points at the failed session and then
// closes it, so every attachSession error path releases the connection
// exactly once.
func abandonAttach(srv *server, session *mcpsdk.ClientSession) {
	srv.mu.Lock()
	if srv.session == session {
		srv.session = nil
	}
	srv.mu.Unlock()
	_ = session.Close()
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

	next := projectTools(srv, res.Tools, srv.resources)

	srv.mu.Lock()
	added, removed := diffTools(srv.tools, next)
	srv.tools = next
	srv.mu.Unlock()

	s.publish(added, removed)
	return nil
}

// projectTools renders a server's tool list into the local projection,
// keeping the first occurrence of each qualified name. MCP requires
// tool names to be unique, but a misbehaving server may still return
// duplicates; deduplicating here keeps srv.tools, the publish deltas,
// and Tools() consistent. Resource bridge tools are appended when
// enabled and lose to a same-named server tool.
func projectTools(srv *server, list []*mcpsdk.Tool, resources bool) []sdktool.Tool {
	next := make([]sdktool.Tool, 0, len(list)+2)
	seen := make(map[string]struct{}, len(list)+2)
	add := func(name string, t sdktool.Tool) {
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		next = append(next, t)
	}
	for _, mt := range list {
		if mt == nil || mt.Name == "" {
			continue
		}
		name := srv.qualify(mt.Name)
		add(name, newAdaptedTool(srv, name, mt))
	}
	if resources {
		for _, spec := range resourceToolSpecs(srv) {
			add(spec.tool.Definition().Name, spec.tool)
		}
	}
	return next
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
	// The first attempt runs immediately; backoff applies only after
	// the first failure.
	backoff := time.Duration(0)
	for {
		select {
		case <-s.baseCtx.Done():
			srv.markReady(errdefs.NotAvailablef("mcp: source is closed"))
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
				srv.markReady(nil)
				s.clearRetrying(srv.name)
				go s.watch(srv)
				return
			}
			// attachSession owns the session on failure: every error
			// path closes it and clears srv.session, so nothing to do
			// here.
			if errdefs.IsValidation(err) {
				s.giveUp(srv, "server rejected attach, giving up", err)
				s.clearRetrying(srv.name)
				return
			}
			if s.baseCtx.Err() != nil {
				srv.markReady(errdefs.NotAvailablef("mcp: source is closed"))
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
			s.giveUp(srv, "server rejected connection, giving up", err)
			s.clearRetrying(srv.name)
			return
		}
		if s.baseCtx.Err() != nil {
			srv.markReady(errdefs.NotAvailablef("mcp: source is closed"))
			s.clearRetrying(srv.name)
			return
		}
		telemetry.WarnErr(s.baseCtx, "mcp: server connect failed, will retry", err,
			otellog.String("server", srv.name))
		backoff = s.nextBackoff(backoff)
	}
}

// giveUp logs a terminal background failure and wakes WaitReady
// waiters with it. Required servers get an explicit mention so a host
// that declared one knows its startup contract was not met.
func (s *Source) giveUp(srv *server, msg string, err error) {
	attrs := []otellog.KeyValue{
		otellog.String("server", srv.name),
		otellog.String(telemetry.AttrErrorMessage, err.Error()),
	}
	if srv.cfg.required {
		attrs = append(attrs, otellog.Bool("required", true))
	}
	telemetry.Error(s.baseCtx, "mcp: "+msg, attrs...)
	srv.markReady(err)
}

// nextBackoff doubles cur up to the configured maximum.
func (s *Source) nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return s.retryInitial
	}
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
