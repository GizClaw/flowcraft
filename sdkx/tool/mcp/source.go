package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/singleflight"
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

	// ownerMu guards qualified-name ownership. A qualified tool name may
	// belong to exactly one server; without this, two servers configured
	// with the same prefix could overwrite each other's registry entries
	// and one server's removal would delete the other's tool.
	ownerMu sync.Mutex
	owners  map[string]string
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
		owners:   make(map[string]string),
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
	deferred  bool
	exposure  dynamic.Exposure
	// tools maps server-side tool names to exposure overrides.
	tools map[string]dynamic.Exposure
	// toolExposure maps qualified registry names to their per-tool
	// exposure; names without an entry use the server-level exposure.
	toolExposure map[string]dynamic.Exposure
	dynamicCat   *dynamic.Catalog
	clientName   string
	clientVer    string
	clientOpts   *mcpsdk.ClientOptions
	onListError  func(server string, err error)
	resources    bool
	// resourceTools tracks qualified names registered by the resource
	// bridge, so a server-side tool with the same name is never
	// shadowed by the bridge.
	resourceTools map[string]struct{}

	mu         sync.Mutex
	session    *mcpsdk.ClientSession
	registered map[string]struct{}
	declared   map[string]struct{}
	loadGroup  singleflight.Group
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
	deferred    bool
	exposure    dynamic.Exposure
	exposureSet bool
	tools       map[string]dynamic.Exposure
	dynamicCat  *dynamic.Catalog
	resources   bool
}

// WithScope sets the registry scope the server's tools register under.
// Defaults to tool.ScopeAgent, i.e. visible to models. Use
// tool.ScopePlatform for a server whose tools should stay hidden from
// tool listings but remain callable by explicit name.
func WithScope(scope string) ServerOption {
	return func(c *serverConfig) { c.scope = scope }
}

// WithDeferred attaches the server without connecting: no child process
// and no tools/list until the first load. Declared tools (see
// WithTools) are registered as lazy proxies immediately; servers
// without a tools map register nothing until loaded.
func WithDeferred(deferred bool) ServerOption {
	return func(c *serverConfig) { c.deferred = deferred }
}

// WithExposure sets the default exposure for every tool this server
// contributes. Per-tool entries in WithTools override it. The default
// is dynamic.ExposureDeferred: MCP tools enter the model's view through
// tool_search, not by dumping every server into the prompt.
func WithExposure(exposure dynamic.Exposure) ServerOption {
	return func(c *serverConfig) {
		c.exposure = exposure
		c.exposureSet = true
	}
}

// WithTools declares per-tool exposure by server-side tool name (not
// the qualified registry name — prefixing stays WithPrefix's job). For
// deferred servers these names are also registered immediately as lazy
// proxies, so they are callable by exact name before the first load.
func WithTools(tools map[string]dynamic.Exposure) ServerOption {
	return func(c *serverConfig) { c.tools = tools }
}

// WithDynamicCatalog wires a dynamic catalog for exposure metadata:
// every tool this server registers gets its exposure recorded there,
// and deferred proxies are registered through the catalog. A host
// using the dynamic injection layer should always pass its session
// catalog here.
func WithDynamicCatalog(cat *dynamic.Catalog) ServerOption {
	return func(c *serverConfig) { c.dynamicCat = cat }
}

// WithResources bridges the server's MCP resources into two registry
// tools — <prefix>list_resources and <prefix>read_resource — so they
// inherit exposure, approval, and every other middleware capability.
func WithResources(enabled bool) ServerOption {
	return func(c *serverConfig) { c.resources = enabled }
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
	exposure := cfg.exposure
	if !cfg.exposureSet {
		exposure = dynamic.ExposureDeferred
	}
	if err := validateExposureConfig(exposure, cfg.tools); err != nil {
		return err
	}

	toolExposure := make(map[string]dynamic.Exposure, len(cfg.tools))
	for remote, exp := range cfg.tools {
		toolExposure[qualify(cfg.prefix, remote)] = exp
	}

	srv := &server{
		name:          name,
		scope:         cfg.scope,
		prefix:        cfg.prefix,
		transport:     transport,
		deferred:      cfg.deferred,
		exposure:      exposure,
		tools:         cfg.tools,
		toolExposure:  toolExposure,
		dynamicCat:    cfg.dynamicCat,
		clientName:    cfg.clientName,
		clientVer:     cfg.clientVer,
		clientOpts:    cfg.clientOpts,
		onListError:   cfg.onListError,
		resources:     cfg.resources,
		resourceTools: make(map[string]struct{}),
		registered:    make(map[string]struct{}),
		declared:      make(map[string]struct{}),
	}

	if cfg.deferred {
		return s.addDeferred(srv)
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
	if cfg.resources {
		if err := s.reconcileResources(ctx, srv); err != nil {
			s.unregisterAll(srv)
			_ = session.Close()
			return err
		}
	}

	s.mu.Lock()
	if s.closed {
		// Close raced us; undo rather than leak the session.
		s.unregisterAll(srv)
		_ = session.Close()
		s.mu.Unlock()
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if _, exists := s.servers[name]; exists {
		s.mu.Unlock()
		s.unregisterAll(srv)
		_ = session.Close()
		return errdefs.Validationf("mcp: server %q is already attached", name)
	}
	s.servers[name] = srv
	s.mu.Unlock()
	return nil
}

// addDeferred publishes a server without connecting. Declared tools
// (WithTools) are pre-claimed and registered as lazy proxies; tools not
// declared appear only after the first load.
func (s *Source) addDeferred(srv *server) error {
	var claimed []string
	for remote := range srv.tools {
		qualified := srv.qualify(remote)
		if !s.claimOwnership(srv.name, qualified) {
			for _, name := range claimed {
				s.releaseOwnership(name)
			}
			return errdefs.Conflictf(
				"mcp: tool %q is already owned by another attached server",
				qualified)
		}
		claimed = append(claimed, qualified)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		for _, name := range claimed {
			s.releaseOwnership(name)
		}
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if _, exists := s.servers[srv.name]; exists {
		s.mu.Unlock()
		for _, name := range claimed {
			s.releaseOwnership(name)
		}
		return errdefs.Validationf("mcp: server %q is already attached", srv.name)
	}
	s.servers[srv.name] = srv
	s.mu.Unlock()

	if err := s.registerDeclaredProxies(srv); err != nil {
		s.mu.Lock()
		if current, ok := s.servers[srv.name]; ok && current == srv {
			delete(s.servers, srv.name)
		}
		s.mu.Unlock()
		for _, name := range claimed {
			s.releaseOwnership(name)
		}
		return err
	}
	return nil
}

// registerDeclaredProxies creates a LazyTool per declared tool name and
// registers it so the tool is callable by exact name before the first
// load. Loaders share the server's singleflight session.
func (s *Source) registerDeclaredProxies(srv *server) error {
	for remote, exp := range srv.tools {
		qualified := srv.qualify(remote)
		loader := s.deferredLoader(srv, remote)
		placeholder := message.Definition{
			Name:        qualified,
			Description: fmt.Sprintf("deferred MCP tool %s/%s; loads on first use", srv.name, remote),
			InputSchema: emptySchema,
		}
		if srv.dynamicCat != nil {
			if err := srv.dynamicCat.RegisterProxy(qualified, loader, exp,
				dynamic.WithPlaceholder(placeholder)); err != nil {
				return err
			}
		} else {
			proxy := dynamic.NewLazyTool(s.registry, qualified, loader,
				dynamic.WithPlaceholder(placeholder))
			s.registry.RegisterWithScope(proxy, srv.scope)
		}
		srv.declared[qualified] = struct{}{}
	}
	return nil
}

// deferredLoader connects the server on first use (singleflight),
// reconciles the registry, and returns the live adapted tool for the
// declared remote name.
func (s *Source) deferredLoader(srv *server, remote string) dynamic.Loader {
	return func(ctx context.Context) (sdktool.Tool, error) {
		if err := s.ensureSession(ctx, srv); err != nil {
			return nil, err
		}
		if err := s.reconcile(ctx, srv); err != nil {
			return nil, err
		}
		if srv.resources {
			if err := s.reconcileResources(ctx, srv); err != nil {
				return nil, err
			}
		}
		qualified := srv.qualify(remote)
		tool, ok := s.registry.Get(qualified)
		if !ok {
			return nil, errdefs.NotAvailablef(
				"mcp: server %q no longer exposes declared tool %q", srv.name, remote)
		}
		if _, isLazy := tool.(*dynamic.LazyTool); isLazy {
			return nil, errdefs.NotAvailablef(
				"mcp: server %q did not list declared tool %q", srv.name, remote)
		}
		return tool, nil
	}
}

// ensureSession connects a deferred server at most once concurrently.
// The first successful session wins; a failed attempt is retried on the
// next call.
func (s *Source) ensureSession(ctx context.Context, srv *server) error {
	srv.mu.Lock()
	if srv.session != nil {
		srv.mu.Unlock()
		return nil
	}
	srv.mu.Unlock()

	_, err, _ := srv.loadGroup.Do(srv.name, func() (any, error) {
		srv.mu.Lock()
		if srv.session != nil {
			srv.mu.Unlock()
			return nil, nil
		}
		srv.mu.Unlock()

		cfg := &serverConfig{
			scope:       srv.scope,
			prefix:      srv.prefix,
			clientName:  srv.clientName,
			clientVer:   srv.clientVer,
			clientOpts:  srv.clientOpts,
			onListError: srv.onListError,
		}
		session, err := s.connect(ctx, srv, cfg)
		if err != nil {
			return nil, err
		}
		srv.mu.Lock()
		if srv.session == nil {
			srv.session = session
		} else {
			_ = session.Close()
		}
		srv.mu.Unlock()
		return nil, nil
	})
	return err
}

func validateExposureConfig(defaultExposure dynamic.Exposure, tools map[string]dynamic.Exposure) error {
	if !defaultExposure.Valid() {
		return errdefs.Validationf(
			"mcp: server exposure %q is invalid", defaultExposure)
	}
	for remote, exp := range tools {
		if strings.TrimSpace(remote) == "" {
			return errdefs.Validationf("mcp: tools map has an empty tool name")
		}
		if !exp.Valid() {
			return errdefs.Validationf(
				"mcp: exposure %q for tool %q is invalid", exp, remote)
		}
	}
	return nil
}

func qualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + name
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
	var claimedNew []string
	for _, mt := range res.Tools {
		if mt == nil || mt.Name == "" {
			continue
		}
		qualified := srv.qualify(mt.Name)
		if _, already := srv.registered[qualified]; already {
			continue
		}
		preOwned := s.ownerIs(srv.name, qualified)
		if !s.claimOwnership(srv.name, qualified) {
			for _, name := range claimedNew {
				s.releaseOwnership(name)
			}
			return errdefs.Conflictf(
				"mcp: tool %q is already owned by another attached server",
				qualified)
		}
		if !preOwned {
			claimedNew = append(claimedNew, qualified)
		}
	}

	for _, mt := range res.Tools {
		if mt == nil || mt.Name == "" {
			continue
		}
		qualified := srv.qualify(mt.Name)
		fresh[qualified] = struct{}{}
		if _, already := srv.registered[qualified]; already {
			continue
		}
		s.registry.RegisterWithScope(newAdaptedTool(srv, qualified, mt), srv.scope)
		s.applyExposure(srv, qualified)
	}
	for previous := range srv.registered {
		if _, still := fresh[previous]; !still {
			s.registry.Unregister(previous)
			s.releaseOwnership(previous)
		}
	}
	srv.registered = fresh
	return nil
}

// reconcileResources validates that the server supports resources and
// registers the two resource-bridge tools. The bridge never shadows a
// server-side tool with the same name; once registered, the bridge
// tools are re-registered on refresh so metadata stays current.
func (s *Source) reconcileResources(ctx context.Context, srv *server) error {
	session, err := srv.currentSession()
	if err != nil {
		return err
	}
	if _, err := session.ListResources(ctx, nil); err != nil {
		return errdefs.NotAvailablef(
			"mcp: server %q: list resources: %v", srv.name, err)
	}
	for _, spec := range resourceToolSpecs(srv) {
		qualified := srv.qualify(spec.remote)

		srv.mu.Lock()
		_, isResource := srv.resourceTools[qualified]
		_, isServerTool := srv.registered[qualified]
		srv.mu.Unlock()
		if !isResource {
			if isServerTool {
				// The server's own tool with this name wins; the bridge
				// stays out of the way.
				continue
			}
			if !s.claimOwnership(srv.name, qualified) {
				return errdefs.Conflictf(
					"mcp: resource tool %q is already owned by another attached server",
					qualified)
			}
			srv.mu.Lock()
			srv.resourceTools[qualified] = struct{}{}
			srv.mu.Unlock()
		}
		s.registry.RegisterWithScope(spec.tool, srv.scope)
		s.applyExposure(srv, qualified)
		srv.mu.Lock()
		srv.registered[qualified] = struct{}{}
		srv.mu.Unlock()
	}
	return nil
}

// claimOwnership records that qualified belongs to serverName. It reports
// false when another server already owns the name.
func (s *Source) claimOwnership(serverName, qualified string) bool {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	if owner, ok := s.owners[qualified]; ok && owner != serverName {
		return false
	}
	s.owners[qualified] = serverName
	return true
}

func (s *Source) releaseOwnership(qualified string) {
	s.ownerMu.Lock()
	delete(s.owners, qualified)
	s.ownerMu.Unlock()
}

// ownerIs reports whether qualified is currently owned by serverName.
func (s *Source) ownerIs(serverName, qualified string) bool {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	return s.owners[qualified] == serverName
}

// applyExposure records a tool's exposure on the wired dynamic catalog.
// Per-tool entries win; everything else uses the server-level default.
func (s *Source) applyExposure(srv *server, qualified string) {
	if srv.dynamicCat == nil {
		return
	}
	_ = srv.dynamicCat.SetExposure(qualified, s.exposureFor(srv, qualified))
}

func (s *Source) exposureFor(srv *server, qualified string) dynamic.Exposure {
	if exp, ok := srv.toolExposure[qualified]; ok {
		return exp
	}
	return srv.exposure
}

// ApplyExposures records every attached server's exposure metadata on a
// dynamic catalog. Hosts that wire the catalog after attachment (e.g.
// through a config source) use this instead of WithDynamicCatalog.
func (s *Source) ApplyExposures(cat *dynamic.Catalog) error {
	if cat == nil {
		return errdefs.Validationf("mcp: ApplyExposures: catalog is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.servers {
		srv.mu.Lock()
		names := make([]string, 0, len(srv.registered)+len(srv.declared))
		for qualified := range srv.registered {
			names = append(names, qualified)
		}
		for qualified := range srv.declared {
			names = append(names, qualified)
		}
		srv.mu.Unlock()
		for _, qualified := range names {
			if err := cat.SetExposure(qualified, s.exposureFor(srv, qualified)); err != nil {
				return err
			}
		}
	}
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
		if srv.deferred {
			if err := s.ensureSession(ctx, srv); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := s.reconcile(ctx, srv); err != nil {
			errs = append(errs, err)
			continue
		}
		if srv.resources {
			if err := s.reconcileResources(ctx, srv); err != nil {
				errs = append(errs, err)
			}
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
	names := make([]string, 0, len(srv.registered)+len(srv.declared))
	for tool := range srv.registered {
		names = append(names, tool)
	}
	for tool := range srv.declared {
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
		s.releaseOwnership(name)
	}
	for name := range srv.declared {
		s.registry.Unregister(name)
		s.releaseOwnership(name)
	}
	srv.registered = make(map[string]struct{})
	srv.declared = make(map[string]struct{})
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
