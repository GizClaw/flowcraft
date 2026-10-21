package server

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// AuditEvent describes one primitive call outcome.
type AuditEvent struct {
	PluginID string
	// Primitive is the "resource.op" id that was called.
	Primitive string
	Outcome   string
	Duration  time.Duration
}

// ServerOptions configures the craft half of the plugin wire.
type ServerOptions struct {
	// Registry is the primitive set every plugin is served. It is built by
	// [Catalog] from the primitives the application contributes.
	Registry *Registry
	// HostVersion is what host.about reports.
	HostVersion string
	// Audit receives one record per primitive call (no payloads).
	Audit func(AuditEvent)
	// RateLimit bounds primitive calls per second per connection; <=0 uses
	// 20.
	RateLimit int
	// RuntimeDir overrides where plugin sockets live. Empty means
	// [RuntimeDir].
	RuntimeDir string
	// MaxCallBytes is the bound Hello advertises on one call's arguments or
	// result; 0 means [DefaultMaxCallBytes].
	MaxCallBytes uint32
}

// DefaultMaxCallBytes is the bound a call's arguments or result may reach
// when nothing else is configured.
const DefaultMaxCallBytes = 8 << 20

// Server is the craft-wide half of the plugin wire: the primitive set and
// the accounting every per-connection Host service shares.
type Server struct {
	registry     *Registry
	hostVersion  string
	audit        func(AuditEvent)
	limit        int
	runtimeDir   string
	maxCallBytes uint32

	mu     sync.Mutex
	conns  map[*Conn]struct{}
	closed bool
}

// NewServer builds the craft-wide server. Connections are opened per plugin
// process with [Server.NewConn].
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Registry == nil {
		return nil, errdefs.Validationf("plugin host: Registry is required")
	}
	dir := opts.RuntimeDir
	if dir == "" {
		computed, err := RuntimeDir()
		if err != nil {
			return nil, err
		}
		dir = computed
	}
	limit := opts.RateLimit
	if limit <= 0 {
		limit = 20
	}
	maxBytes := opts.MaxCallBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxCallBytes
	}
	return &Server{
		registry:     opts.Registry,
		hostVersion:  opts.HostVersion,
		audit:        opts.Audit,
		limit:        limit,
		runtimeDir:   dir,
		maxCallBytes: maxBytes,
		conns:        make(map[*Conn]struct{}),
	}, nil
}

// Registry returns the primitive set.
func (s *Server) Registry() *Registry { return s.registry }

// RuntimeDir is where this server opens its sockets.
func (s *Server) RuntimeDir() string { return s.runtimeDir }

// Connections reports how many plugin connections the server holds open.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Close tears every connection down. A plugin process that outlived its
// connection leaves nothing behind, because the only thing it held was the
// file descriptor.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]*Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	var errs []error
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) track(conn *Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(conn *Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// removeSocket unlinks one socket path. A path that is already gone is not a
// failure: the plugin or the OS may have removed it first.
func removeSocket(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
