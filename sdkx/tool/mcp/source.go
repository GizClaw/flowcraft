package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPrefixSeparator joins a server name to a tool name when
// namespacing is enabled. Double underscore is the convention MCP hosts
// converged on: it survives the `^[a-zA-Z0-9_-]+$` name restriction
// every major provider enforces on function-calling schemas, and it is
// unlikely to collide with a server's own naming.
const DefaultPrefixSeparator = "__"

// Source attaches MCP servers to a tool.Registry.
//
// It is not a Catalog. Discovered tools are registered into the host's
// existing registry, which means an MCP tool and a hand-written Go tool
// are the same kind of thing to every consumer downstream — the
// inference node resolving `tools: [...]` by name, the tool node
// dispatching a call, the script bridge's allow-list. There is
// deliberately no second dispatch path and no second catalog to keep in
// sync.
//
// Discovery results are projected into the registry rather than fetched
// on demand because Catalog.Definitions() is synchronous, infallible,
// and called once per LLM turn; reaching out to every attached server on
// that path would make one slow server stall unrelated work. The go-sdk
// maintains its own TTL cache for tools/list and invalidates it when the
// server sends tools/list_changed, so re-listing is cheap and Source
// adds no caching of its own — it only reconciles the registry when the
// server says the list moved.
//
// A Source is host-owned and must be closed. Close is what releases
// child processes and HTTP sessions.
type Source struct {
	registry *sdktool.Registry

	mu      sync.Mutex
	servers map[string]*server
	closed  bool
}

// NewSource returns a Source that registers discovered tools into
// registry. A nil registry is a programming bug and panics, mirroring
// tool.NewExecutor.
func NewSource(registry *sdktool.Registry) *Source {
	if registry == nil {
		panic("mcp.NewSource: registry is nil")
	}
	return &Source{
		registry: registry,
		servers:  make(map[string]*server),
	}
}

// server is one live connection plus the registry footprint it owns.
// registered tracks the qualified names this server last put into the
// registry so a later reconcile can retract the ones that disappeared
// without touching another server's tools.
type server struct {
	name      string
	scope     string
	prefix    string
	transport mcpsdk.Transport

	mu         sync.Mutex
	session    *mcpsdk.ClientSession
	registered map[string]struct{}
}

// currentSession returns the live session or a typed error if the
// server has been detached. Adapted tools hold a *server rather than a
// session so that RemoveServer immediately makes in-flight tools fail
// loudly instead of talking to a closed connection.
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
	scope       string
	prefix      string
	prefixSet   bool
	clientName  string
	clientVer   string
	clientOpts  *mcpsdk.ClientOptions
	onListError func(server string, err error)
}

// WithScope sets the registry scope the server's tools register under.
// Defaults to tool.ScopeAgent, i.e. visible to models. Use
// tool.ScopePlatform for a server whose tools should stay hidden from
// tool listings but remain callable by explicit name.
func WithScope(scope string) ServerOption {
	return func(c *serverConfig) { c.scope = scope }
}

// WithPrefix overrides the namespace prefix applied to the server's tool
// names. The default is "<serverName>__". Passing an empty string
// registers tools under their bare server-side names, which is only safe
// when the host knows the names cannot collide with another server or a
// built-in tool.
func WithPrefix(prefix string) ServerOption {
	return func(c *serverConfig) {
		c.prefix = prefix
		c.prefixSet = true
	}
}

// WithClientInfo sets the client identity reported to the server during
// initialization. Servers surface it in their logs and some gate
// behaviour on it.
func WithClientInfo(name, version string) ServerOption {
	return func(c *serverConfig) {
		c.clientName = name
		c.clientVer = version
	}
}

// WithClientOptions supplies go-sdk client options verbatim, for
// features this package does not wrap (elicitation, progress,
// keepalive). The ToolListChangedHandler is chained rather than
// replaced: Source needs it to reconcile the registry, and the caller's
// handler still runs afterwards.
func WithClientOptions(opts *mcpsdk.ClientOptions) ServerOption {
	return func(c *serverConfig) { c.clientOpts = opts }
}

// WithListErrorHandler installs a callback for tools/list failures that
// happen outside a caller's control — specifically, the reconcile
// triggered by a tools/list_changed notification. Without it those
// errors have nowhere to go, since no caller is on the stack.
func WithListErrorHandler(fn func(server string, err error)) ServerOption {
	return func(c *serverConfig) { c.onListError = fn }
}

// AddServer connects to one MCP server, discovers its tools, and
// registers them.
//
// The call is synchronous: when it returns without error the registry
// already holds the server's tools, so a host can attach servers during
// setup and know its registry is complete before serving traffic. A
// connect or initial-list failure leaves the registry untouched and the
// transport closed.
//
// Tool names are namespaced with "<name>__" by default so two servers
// exposing the same tool coexist; see WithPrefix to change that.
func (s *Source) AddServer(ctx context.Context, name string, transport mcpsdk.Transport, opts ...ServerOption) error {
	if strings.TrimSpace(name) == "" {
		return errdefs.Validationf("mcp: server name is empty")
	}
	if transport == nil {
		return errdefs.Validationf("mcp: server %q: transport is nil", name)
	}

	cfg := &serverConfig{
		scope:      sdktool.ScopeAgent,
		prefix:     name + DefaultPrefixSeparator,
		clientName: "flowcraft",
		clientVer:  "v1",
	}
	for _, opt := range opts {
		opt(cfg)
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
	s.mu.Unlock()

	srv := &server{
		name:       name,
		scope:      cfg.scope,
		prefix:     cfg.prefix,
		transport:  transport,
		registered: make(map[string]struct{}),
	}

	session, err := s.connect(ctx, srv, cfg)
	if err != nil {
		return err
	}
	srv.session = session

	// Reconcile before publishing the server so a failed initial list
	// cannot leave a half-registered server behind.
	if err := s.reconcile(ctx, srv); err != nil {
		_ = session.Close()
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Close raced us; undo rather than leak the session.
		s.unregisterAll(srv)
		_ = session.Close()
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	s.servers[name] = srv
	return nil
}

// connect builds the go-sdk client with the reconcile hook wired and
// performs initialization.
func (s *Source) connect(ctx context.Context, srv *server, cfg *serverConfig) (*mcpsdk.ClientSession, error) {
	opts := mcpsdk.ClientOptions{}
	if cfg.clientOpts != nil {
		opts = *cfg.clientOpts
	}
	userHandler := opts.ToolListChangedHandler
	opts.ToolListChangedHandler = func(ctx context.Context, req *mcpsdk.ToolListChangedRequest) {
		// The go-sdk has already invalidated its tools cache by the
		// time this runs, so the re-list below sees fresh data.
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

// reconcile re-lists the server's tools and makes the registry match.
//
// New and changed tools are re-registered (registration is overwrite
// semantics, so a changed input schema takes effect without an
// intervening unregister) and vanished tools are removed. A list failure
// leaves the previous projection in place: a transient outage should not
// make the model suddenly lose sight of tools it was told about, and the
// per-call error path already reports the server as unavailable.
func (s *Source) reconcile(ctx context.Context, srv *server) error {
	session, err := srv.currentSession()
	if err != nil {
		return err
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return errdefs.NotAvailablef("mcp: server %q: list tools: %v", srv.name, err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	fresh := make(map[string]struct{}, len(res.Tools))
	for _, mt := range res.Tools {
		if mt == nil || mt.Name == "" {
			continue
		}
		qualified := srv.qualify(mt.Name)
		fresh[qualified] = struct{}{}
		s.registry.RegisterWithScope(newAdaptedTool(srv, qualified, mt), srv.scope)
	}
	for previous := range srv.registered {
		if _, still := fresh[previous]; !still {
			s.registry.Unregister(previous)
		}
	}
	srv.registered = fresh
	return nil
}

// Refresh re-lists every attached server and reconciles the registry.
//
// Hosts need this for servers that do not send tools/list_changed
// notifications. Servers are refreshed independently: one failing server
// does not prevent the others from updating, and every error is
// returned joined so the caller sees the full picture.
func (s *Source) Refresh(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	attached := make([]*server, 0, len(s.servers))
	for _, srv := range s.servers {
		attached = append(attached, srv)
	}
	s.mu.Unlock()

	var errs []error
	for _, srv := range attached {
		if err := s.reconcile(ctx, srv); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveServer detaches one server: its tools leave the registry and its
// session (and therefore its child process or HTTP session) closes.
// Tools already handed out fail with a typed NotAvailable error rather
// than reaching a closed connection.
func (s *Source) RemoveServer(name string) error {
	s.mu.Lock()
	srv, ok := s.servers[name]
	if !ok {
		s.mu.Unlock()
		return errdefs.NotFoundf("mcp: server %q is not attached", name)
	}
	delete(s.servers, name)
	s.mu.Unlock()

	s.unregisterAll(srv)
	return closeServer(srv)
}

// Servers returns the names of the attached servers, for operators and
// diagnostics.
func (s *Source) Servers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.servers))
	for name := range s.servers {
		names = append(names, name)
	}
	return names
}

// ToolNames returns the registry keys currently owned by server name.
func (s *Source) ToolNames(name string) []string {
	s.mu.Lock()
	srv, ok := s.servers[name]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	names := make([]string, 0, len(srv.registered))
	for tool := range srv.registered {
		names = append(names, tool)
	}
	return names
}

// Close detaches every server: all tools are unregistered and all
// sessions closed. It is idempotent, and errors from individual servers
// are joined so one stuck child process does not hide the rest.
func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	attached := make([]*server, 0, len(s.servers))
	for _, srv := range s.servers {
		attached = append(attached, srv)
	}
	s.servers = make(map[string]*server)
	s.mu.Unlock()

	var errs []error
	for _, srv := range attached {
		s.unregisterAll(srv)
		if err := closeServer(srv); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// unregisterAll retracts a server's whole registry footprint.
func (s *Source) unregisterAll(srv *server) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for name := range srv.registered {
		s.registry.Unregister(name)
	}
	srv.registered = make(map[string]struct{})
}

// closeServer clears the session pointer before closing so concurrent
// Execute calls see "not connected" instead of racing a teardown.
func closeServer(srv *server) error {
	srv.mu.Lock()
	session := srv.session
	srv.session = nil
	srv.mu.Unlock()
	if session == nil {
		return nil
	}
	if err := session.Close(); err != nil {
		return fmt.Errorf("mcp: server %q: close session: %w", srv.name, err)
	}
	return nil
}
