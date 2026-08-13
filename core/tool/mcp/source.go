package mcp

import (
	"context"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPrefixSeparator joins a server name to a tool name when
// namespacing is enabled.
const DefaultPrefixSeparator = "__"

// Source is a tool.Source that connects MCP servers and exposes their
// tools as ordinary core/tool.Tool values. It connects eagerly at
// AddServer time, so the tool list is complete before the Source is
// handed to a Registry or Assembly.
type Source struct {
	mu      sync.Mutex
	servers map[string]*server
	closed  bool
}

// NewSource returns an empty MCP Source.
func NewSource() *Source {
	return &Source{servers: make(map[string]*server)}
}

// Tools implements tool.Source: every discovered tool plus optional
// resource-bridge tools, in server attach order.
func (s *Source) Tools() []tool.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []tool.Tool
	for _, srv := range s.servers {
		out = append(out, srv.tools...)
	}
	return out
}

// LazyTools implements tool.Source. MCP tools are discovered eagerly, so
// there are no lazy entries.
func (s *Source) LazyTools() []tool.LazyTool { return nil }

// server is one live connection plus its exposed tools.
type server struct {
	name      string
	prefix    string
	transport mcpsdk.Transport

	clientName  string
	clientVer   string
	clientOpts  *mcpsdk.ClientOptions
	onListError func(server string, err error)
	resources   bool

	mu      sync.Mutex
	session *mcpsdk.ClientSession
	tools   []tool.Tool
}

// currentSession returns the live session or a typed error if the
// server has been detached.
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

// AddServer connects to one MCP server, discovers its tools, and stores
// them on the Source.
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
		name:        name,
		prefix:      cfg.prefix,
		transport:   transport,
		clientName:  cfg.clientName,
		clientVer:   cfg.clientVer,
		clientOpts:  cfg.clientOpts,
		onListError: cfg.onListError,
		resources:   cfg.resources,
	}
	session, err := s.connect(ctx, srv, cfg)
	if err != nil {
		return err
	}
	srv.mu.Lock()
	srv.session = session
	srv.mu.Unlock()

	if err := s.reconcile(ctx, srv); err != nil {
		_ = session.Close()
		return err
	}
	if cfg.resources {
		for _, spec := range resourceToolSpecs(srv) {
			srv.tools = append(srv.tools, spec.tool)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = session.Close()
		return errdefs.NotAvailablef("mcp: source is closed")
	}
	if _, exists := s.servers[name]; exists {
		_ = session.Close()
		return errdefs.Validationf("mcp: server %q is already attached", name)
	}
	s.servers[name] = srv
	return nil
}

// Close releases all sessions. Idempotent.
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

// reconcile re-lists the server's tools and stores adapted tools.
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
	srv.tools = srv.tools[:0]
	for _, mt := range res.Tools {
		if mt == nil || mt.Name == "" {
			continue
		}
		srv.tools = append(srv.tools, newAdaptedTool(srv, srv.qualify(mt.Name), mt))
	}
	return nil
}

var _ tool.Source = (*Source)(nil)
