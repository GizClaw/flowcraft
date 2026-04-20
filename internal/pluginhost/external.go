package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/plugin"
	pb "github.com/GizClaw/flowcraft/plugin/proto"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ExternalPlugin wraps an external plugin subprocess.
// Communication uses gRPC over Unix domain socket.
type ExternalPlugin struct {
	info       plugin.PluginInfo
	path       string
	cmd        *exec.Cmd
	config     map[string]any
	cancel     context.CancelFunc
	healthy    bool
	mu         sync.Mutex
	conn       *grpc.ClientConn
	lifecycle  pb.PluginLifecycleClient
	toolSvc    pb.ToolServiceClient
	nodeSvc    pb.NodeServiceClient
	socketPath string
	tools      []plugin.ToolSpec
	nodes      []plugin.NodeSpec
}

// Info returns the plugin metadata.
func (ep *ExternalPlugin) Info() plugin.PluginInfo { return ep.info }

// Tools returns the tool specs advertised during handshake.
func (ep *ExternalPlugin) Tools() []plugin.ToolSpec { return ep.tools }

// Nodes returns the node specs advertised during handshake.
func (ep *ExternalPlugin) Nodes() []plugin.NodeSpec { return ep.nodes }

// SocketPath returns the Unix socket path used for gRPC communication.
func (ep *ExternalPlugin) SocketPath() string { return ep.socketPath }

// NewExternalPlugin creates an ExternalPlugin with the given path and info.
// This is used by bootstrap to construct plugins from plugins.json config.
func NewExternalPlugin(path string, info plugin.PluginInfo) *ExternalPlugin {
	return &ExternalPlugin{
		path: path,
		info: info,
	}
}

// ToolClient returns a ToolServiceClient backed by the gRPC connection.
func (ep *ExternalPlugin) ToolClient() plugin.ToolServiceClient {
	if ep.toolSvc == nil {
		return nil
	}
	return &grpcToolClient{client: ep.toolSvc}
}

// NodeClient returns a NodeServiceClient backed by the gRPC connection.
func (ep *ExternalPlugin) NodeClient() plugin.NodeServiceClient {
	if ep.nodeSvc == nil {
		return nil
	}
	return &grpcNodeClient{client: ep.nodeSvc}
}

// Initialize starts the external plugin subprocess, connects via gRPC Unix Socket,
// and performs handshake.
func (ep *ExternalPlugin) Initialize(ctx context.Context, config map[string]any) error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.config = config
	pluginCtx, cancel := context.WithCancel(ctx)
	ep.cancel = cancel

	// Unix socket path in temp dir
	ep.socketPath = filepath.Join(os.TempDir(), fmt.Sprintf("flowcraft-plugin-%s-%d.sock", ep.info.ID, time.Now().UnixNano()))
	_ = os.Remove(ep.socketPath)

	ep.cmd = exec.CommandContext(pluginCtx, ep.path)
	ep.cmd.Env = append(os.Environ(), "FLOWCRAFT_SOCKET="+ep.socketPath)
	ep.cmd.Stderr = os.Stderr

	if err := ep.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("plugin: start %q: %w", ep.path, err)
	}

	// Wait for the socket to appear (plugin starts gRPC server)
	if err := waitForSocket(ctx, ep.socketPath, 10*time.Second); err != nil {
		cancel()
		_ = ep.cmd.Process.Kill()
		return fmt.Errorf("plugin: socket wait %q: %w", ep.path, err)
	}

	conn, err := grpc.NewClient(
		"unix://"+ep.socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		_ = ep.cmd.Process.Kill()
		return fmt.Errorf("plugin: grpc dial %q: %w", ep.path, err)
	}
	ep.conn = conn
	ep.lifecycle = pb.NewPluginLifecycleClient(conn)
	ep.toolSvc = pb.NewToolServiceClient(conn)
	ep.nodeSvc = pb.NewNodeServiceClient(conn)

	hsCtx, hsCancel := context.WithTimeout(ctx, 10*time.Second)
	defer hsCancel()

	hsResp, err := ep.lifecycle.Handshake(hsCtx, &pb.HandshakeRequest{
		HostVersion:     "1.0.0",
		ProtocolVersion: 1,
	})
	if err != nil {
		_ = conn.Close()
		cancel()
		_ = ep.cmd.Process.Kill()
		return fmt.Errorf("plugin: handshake %q: %w", ep.path, err)
	}

	if hsResp.PluginInfo != nil {
		ep.info = pluginInfoFromProto(hsResp.PluginInfo)
	}
	ep.tools = toolSpecsFromProto(hsResp.Tools)
	ep.nodes = nodeSpecsFromProto(hsResp.Nodes)
	ep.healthy = true

	telemetry.Info(ctx, "plugin: external started",
		otellog.String("id", ep.info.ID),
		otellog.String("path", ep.path),
		otellog.Int("pid", ep.cmd.Process.Pid),
		otellog.Int("tools", len(ep.tools)),
		otellog.Int("nodes", len(ep.nodes)))

	return nil
}

// Shutdown terminates the external plugin subprocess and cleans up.
func (ep *ExternalPlugin) Shutdown(ctx context.Context) error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	ep.healthy = false

	// Graceful gRPC shutdown
	if ep.lifecycle != nil {
		shutCtx, shutCancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = ep.lifecycle.Shutdown(shutCtx, &pb.ShutdownRequest{})
		shutCancel()
	}

	if ep.conn != nil {
		_ = ep.conn.Close()
		ep.conn = nil
	}

	if ep.cancel != nil {
		ep.cancel()
	}

	if ep.cmd != nil && ep.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- ep.cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = ep.cmd.Process.Kill()
			<-done
		}
	}

	if ep.socketPath != "" {
		_ = os.Remove(ep.socketPath)
	}

	telemetry.Info(ctx, "plugin: external stopped", otellog.String("id", ep.info.ID))
	return nil
}

// waitForSocket polls until the Unix socket file appears or timeout.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for socket %q", path)
		case <-ticker.C:
			if conn, err := net.DialTimeout("unix", path, 100*time.Millisecond); err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
}

// Proto → Go type converters

func pluginInfoFromProto(p *pb.PluginInfo) plugin.PluginInfo {
	return plugin.PluginInfo{
		ID:          p.Id,
		Name:        p.Name,
		Version:     p.Version,
		Type:        plugin.PluginType(p.Type),
		Description: p.Description,
		Author:      p.Author,
		Builtin:     p.Builtin,
	}
}

func toolSpecsFromProto(specs []*pb.ToolSpec) []plugin.ToolSpec {
	result := make([]plugin.ToolSpec, 0, len(specs))
	for _, s := range specs {
		ts := plugin.ToolSpec{
			Name:        s.Name,
			Description: s.Description,
		}
		if s.InputSchemaJson != "" {
			var schema map[string]any
			if err := json.Unmarshal([]byte(s.InputSchemaJson), &schema); err == nil {
				ts.InputSchema = schema
			}
		}
		result = append(result, ts)
	}
	return result
}

func nodeSpecsFromProto(specs []*pb.NodeSpec) []plugin.NodeSpec {
	result := make([]plugin.NodeSpec, 0, len(specs))
	for _, s := range specs {
		ns := plugin.NodeSpec{Type: s.Type}
		if s.SchemaJson != "" {
			var schema map[string]any
			if err := json.Unmarshal([]byte(s.SchemaJson), &schema); err == nil {
				ns.Schema = schema
			}
		}
		result = append(result, ns)
	}
	return result
}

// ExternalManager manages discovery, lifecycle, and health of external plugins.
type ExternalManager struct {
	pluginDir      string
	plugins        map[string]*ExternalPlugin
	mu             sync.RWMutex
	healthInterval time.Duration
	maxFailures    int
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// ExternalManagerConfig configures the external plugin manager.
type ExternalManagerConfig struct {
	PluginDir      string        `json:"plugin_dir"`
	HealthInterval time.Duration `json:"health_interval,omitempty"`
	MaxFailures    int           `json:"max_failures,omitempty"`
}

// PluginDir returns the directory scanned for external plugin binaries.
func (m *ExternalManager) PluginDir() string { return m.pluginDir }

// NewExternalManager creates an external plugin manager.
func NewExternalManager(cfg ExternalManagerConfig) *ExternalManager {
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 10 * time.Second
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 3
	}
	return &ExternalManager{
		pluginDir:      cfg.PluginDir,
		plugins:        make(map[string]*ExternalPlugin),
		healthInterval: cfg.HealthInterval,
		maxFailures:    cfg.MaxFailures,
	}
}

// Discover scans the plugin directory for executable plugin binaries.
func (m *ExternalManager) Discover() ([]*ExternalPlugin, error) {
	if m.pluginDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(m.pluginDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: scan dir %q: %w", m.pluginDir, err)
	}

	var found []*ExternalPlugin
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(m.pluginDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}

		ep := &ExternalPlugin{
			info: plugin.PluginInfo{
				ID:      entry.Name(),
				Name:    entry.Name(),
				Version: "0.0.0",
			},
			path: path,
		}
		found = append(found, ep)
	}

	return found, nil
}

// Start begins health check monitoring.
func (m *ExternalManager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.healthLoop(ctx)
	}()
}

// Stop terminates all external plugins and the health monitor.
func (m *ExternalManager) Stop(ctx context.Context) {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ep := range m.plugins {
		_ = ep.Shutdown(ctx)
	}
}

// Register adds an external plugin to the manager.
func (m *ExternalManager) Register(ep *ExternalPlugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plugins[ep.info.ID] = ep
}

// Get returns an external plugin by ID.
func (m *ExternalManager) Get(id string) (*ExternalPlugin, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ep, ok := m.plugins[id]
	return ep, ok
}

func (m *ExternalManager) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()

	failures := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var toRestart []string

			m.mu.RLock()
			for id, ep := range m.plugins {
				ep.mu.Lock()
				if ep.cmd == nil || ep.cmd.Process == nil {
					ep.mu.Unlock()
					continue
				}
				if ep.cmd.ProcessState != nil && ep.cmd.ProcessState.Exited() {
					ep.healthy = false
					failures[id]++
					telemetry.Warn(ctx, "plugin: health check failed",
						otellog.String("id", id),
						otellog.Int("failures", failures[id]))
					if failures[id] >= m.maxFailures {
						toRestart = append(toRestart, id)
					}
				} else {
					if ep.lifecycle != nil {
						hcCtx, hcCancel := context.WithTimeout(ctx, 2*time.Second)
						_, hcErr := ep.lifecycle.HealthCheck(hcCtx, &pb.HealthCheckRequest{})
						hcCancel()
						if hcErr != nil {
							ep.healthy = false
							failures[id]++
						} else {
							failures[id] = 0
						}
					} else {
						failures[id] = 0
					}
				}
				ep.mu.Unlock()
			}
			m.mu.RUnlock()

			for _, id := range toRestart {
				telemetry.Error(ctx, "plugin: max failures reached, restarting",
					otellog.String("id", id))
				m.mu.RLock()
				ep, ok := m.plugins[id]
				m.mu.RUnlock()
				if ok {
					if err := ep.Initialize(ctx, ep.config); err != nil {
						telemetry.Error(ctx, "plugin: restart failed",
							otellog.String("id", id),
							otellog.String("error", err.Error()))
					} else {
						failures[id] = 0
					}
				}
			}
		}
	}
}
