package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/internal/errcode"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// managedPlugin tracks a plugin instance with its runtime state.
type managedPlugin struct {
	p      plugin.Plugin
	status plugin.PluginStatus
	config map[string]any
	errMsg string
}

// Registry manages plugin lifecycle: register, enable, disable, config update.
// Supports both built-in (Builtin=true) and external (Builtin=false) plugins.
//
// Thread safety: all public methods are safe for concurrent use.
type Registry struct {
	mu          sync.RWMutex
	plugins     map[string]*managedPlugin
	nodePlugins map[string]string // nodeType → pluginID
	extManager  *ExternalManager
}

// NewRegistry creates a plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins:     make(map[string]*managedPlugin),
		nodePlugins: make(map[string]string),
	}
}

// SetExternalManager configures the external plugin manager.
func (r *Registry) SetExternalManager(m *ExternalManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extManager = m
}

// Register adds a plugin (typically built-in, called from init()).
func (r *Registry) Register(p plugin.Plugin) error {
	return r.RegisterWithConfig(p, nil)
}

// RegisterWithConfig adds a plugin with an optional initial config.
func (r *Registry) RegisterWithConfig(p plugin.Plugin, config map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	info := p.Info()
	if _, exists := r.plugins[info.ID]; exists {
		return errdefs.Conflictf("plugin %q already registered", info.ID)
	}

	mp := &managedPlugin{
		p:      p,
		status: plugin.StatusInstalled,
		config: config,
	}
	r.plugins[info.ID] = mp

	if np, ok := p.(plugin.NodePlugin); ok {
		r.nodePlugins[np.NodeType()] = info.ID
	}

	return nil
}

// Get returns a plugin by ID.
func (r *Registry) Get(pluginID string) (plugin.Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mp, ok := r.plugins[pluginID]
	if !ok {
		return nil, false
	}
	return mp.p, true
}

// HasNodeType reports whether a plugin provides the given node type.
func (r *Registry) HasNodeType(nodeType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.nodePlugins[nodeType]
	return ok
}

// GetPluginForNodeType returns the plugin that provides the given node type.
func (r *Registry) GetPluginForNodeType(nodeType string) (plugin.Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pluginID, ok := r.nodePlugins[nodeType]
	if !ok {
		return nil, false
	}
	mp, ok := r.plugins[pluginID]
	if !ok {
		return nil, false
	}
	return mp.p, true
}

// GetNodePlugin returns the active NodePlugin for a given node type.
func (r *Registry) GetNodePlugin(nodeType string) plugin.NodePlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pluginID, ok := r.nodePlugins[nodeType]
	if !ok {
		return nil
	}
	mp, ok := r.plugins[pluginID]
	if !ok || mp.status != plugin.StatusActive {
		return nil
	}
	np, _ := mp.p.(plugin.NodePlugin)
	return np
}

// List returns all installed plugins.
func (r *Registry) List() []plugin.InstalledPlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]plugin.InstalledPlugin, 0, len(r.plugins))
	for _, mp := range r.plugins {
		result = append(result, plugin.InstalledPlugin{
			Info:   mp.p.Info(),
			Status: mp.status,
			Config: mp.config,
			Error:  mp.errMsg,
		})
	}
	return result
}

// Enable activates a plugin. Built-in plugins cannot be manually enabled/disabled.
func (r *Registry) Enable(ctx context.Context, pluginID string, config map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	mp, ok := r.plugins[pluginID]
	if !ok {
		return errdefs.NotFoundf("plugin %s not found", pluginID)
	}

	if mp.p.Info().Builtin {
		return errcode.MethodNotAllowedf("cannot manually enable built-in plugin %q", pluginID)
	}

	if err := mp.p.Initialize(ctx, config); err != nil {
		mp.status = plugin.StatusError
		mp.errMsg = err.Error()
		return errcode.PluginErrorWrap(err, "initialize plugin %q", pluginID)
	}

	mp.status = plugin.StatusActive
	mp.config = config
	mp.errMsg = ""
	return nil
}

// Disable deactivates a plugin. Built-in plugins cannot be disabled.
func (r *Registry) Disable(ctx context.Context, pluginID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	mp, ok := r.plugins[pluginID]
	if !ok {
		return errdefs.NotFoundf("plugin %s not found", pluginID)
	}

	if mp.p.Info().Builtin {
		return errcode.MethodNotAllowedf("cannot disable built-in plugin %q", pluginID)
	}

	if mp.status == plugin.StatusActive {
		if err := mp.p.Shutdown(ctx); err != nil {
			telemetry.Warn(ctx, "plugin: shutdown error",
				otellog.String("id", pluginID),
				otellog.String("error", err.Error()))
		}
	}

	mp.status = plugin.StatusInactive
	return nil
}

// UpdateConfig updates an external plugin's config by Shutdown→Initialize.
func (r *Registry) UpdateConfig(ctx context.Context, pluginID string, config map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	mp, ok := r.plugins[pluginID]
	if !ok {
		return errdefs.NotFoundf("plugin %s not found", pluginID)
	}

	if mp.p.Info().Builtin {
		return errcode.MethodNotAllowedf("cannot update config of built-in plugin %q", pluginID)
	}

	if mp.status == plugin.StatusActive {
		_ = mp.p.Shutdown(ctx)
	}

	if err := mp.p.Initialize(ctx, config); err != nil {
		mp.status = plugin.StatusError
		mp.errMsg = err.Error()
		return errcode.PluginErrorWrap(err, "re-initialize plugin %q", pluginID)
	}

	mp.status = plugin.StatusActive
	mp.config = config
	mp.errMsg = ""
	return nil
}

// InitializeAll initializes all registered plugins (built-in + external).
// For external plugins: discovers binaries via ExternalManager, starts them,
// and registers their proxy tools/nodes/schemas.
func (r *Registry) InitializeAll(ctx context.Context) error {
	r.mu.Lock()

	for id, mp := range r.plugins {
		if mp.status == plugin.StatusActive {
			continue
		}
		if err := mp.p.Initialize(ctx, mp.config); err != nil {
			mp.status = plugin.StatusError
			mp.errMsg = err.Error()
			telemetry.Warn(ctx, "plugin: init failed",
				otellog.String("id", id),
				otellog.String("error", err.Error()))
			continue
		}
		mp.status = plugin.StatusActive
		if ep, ok := mp.p.(*ExternalPlugin); ok && ep.NodeClient() != nil {
			for _, ns := range ep.Nodes() {
				r.nodePlugins[ns.Type] = id
			}
		}
	}

	extMgr := r.extManager
	r.mu.Unlock()

	if extMgr != nil {
		r.discoverAndRegisterExternal(ctx, extMgr)
	}

	return nil
}

// discoverAndRegisterExternal discovers external plugin binaries, starts them,
// and registers them (along with their tools/nodes) in the registry.
func (r *Registry) discoverAndRegisterExternal(ctx context.Context, extMgr *ExternalManager) {
	discovered, err := extMgr.Discover()
	if err != nil {
		telemetry.Warn(ctx, "plugin: discover external failed", otellog.String("error", err.Error()))
		return
	}

	for _, ep := range discovered {
		extMgr.Register(ep)
		if err := ep.Initialize(ctx, nil); err != nil {
			telemetry.Warn(ctx, "plugin: external init failed",
				otellog.String("id", ep.Info().ID),
				otellog.String("error", err.Error()))
			continue
		}

		r.mu.Lock()
		r.plugins[ep.Info().ID] = &managedPlugin{
			p:      ep,
			status: plugin.StatusActive,
		}
		if ep.NodeClient() != nil {
			for _, ns := range ep.Nodes() {
				r.nodePlugins[ns.Type] = ep.Info().ID
			}
		}
		r.mu.Unlock()

		telemetry.Info(ctx, "plugin: registered external plugin",
			otellog.String("id", ep.Info().ID),
			otellog.String("type", string(ep.Info().Type)))
	}

	extMgr.Start(ctx)
}

// ShutdownAll shuts down all active plugins.
func (r *Registry) ShutdownAll(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, mp := range r.plugins {
		if mp.status != plugin.StatusActive {
			continue
		}
		if err := mp.p.Shutdown(ctx); err != nil {
			telemetry.Warn(ctx, "plugin: shutdown error",
				otellog.String("id", id),
				otellog.String("error", err.Error()))
		}
		mp.status = plugin.StatusInactive
	}
}

// Reload re-scans the plugin directory, adds newly discovered plugins and
// removes plugins whose binaries no longer exist. Returns the IDs of plugins
// that were added and removed respectively.
func (r *Registry) Reload(ctx context.Context) (added, removed []string, err error) {
	r.mu.RLock()
	extMgr := r.extManager
	r.mu.RUnlock()

	added = []string{}
	removed = []string{}

	if extMgr == nil {
		return added, removed, nil
	}

	discovered, err := extMgr.Discover()
	if err != nil {
		return added, removed, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	discoveredIDs := make(map[string]*ExternalPlugin)
	for _, ep := range discovered {
		discoveredIDs[ep.info.ID] = ep
	}

	for id, mp := range r.plugins {
		if mp.p.Info().Builtin {
			continue
		}
		if _, ok := discoveredIDs[id]; !ok {
			_ = mp.p.Shutdown(ctx)
			delete(r.plugins, id)
			for nodeType, pid := range r.nodePlugins {
				if pid == id {
					delete(r.nodePlugins, nodeType)
				}
			}
			removed = append(removed, id)
		}
	}

	for id, ep := range discoveredIDs {
		if _, exists := r.plugins[id]; exists {
			continue
		}
		if initErr := ep.Initialize(ctx, nil); initErr != nil {
			telemetry.Warn(ctx, "plugin: reload init failed",
				otellog.String("id", id),
				otellog.String("error", initErr.Error()))
			continue
		}
		r.plugins[id] = &managedPlugin{p: ep, status: plugin.StatusActive}
		extMgr.Register(ep)
		if ep.NodeClient() != nil {
			for _, ns := range ep.Nodes() {
				r.nodePlugins[ns.Type] = id
			}
		}
		added = append(added, id)
	}

	return added, removed, nil
}

// InstallBinary writes binary content to the plugin directory under the given
// filename, marks it executable, and runs Reload to pick it up. Returns the
// plugins that were added / removed by the subsequent reload.
//
// The filename is sanitized: any path separators are rejected, and a leading
// dot is disallowed to prevent hidden-file uploads.
func (r *Registry) InstallBinary(ctx context.Context, filename string, content io.Reader) (added, removed []string, written int64, err error) {
	r.mu.RLock()
	extMgr := r.extManager
	r.mu.RUnlock()

	if extMgr == nil || extMgr.PluginDir() == "" {
		return nil, nil, 0, errdefs.Forbiddenf("external plugin directory is not configured")
	}

	raw := strings.TrimSpace(filename)
	if raw == "" || strings.ContainsAny(raw, `/\`) || raw == "." || raw == ".." || strings.HasPrefix(raw, ".") {
		return nil, nil, 0, errdefs.Validationf("invalid plugin filename %q", filename)
	}
	cleaned := filepath.Base(raw)

	dir := extMgr.PluginDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, 0, fmt.Errorf("plugin: ensure dir %q: %w", dir, err)
	}

	dst := filepath.Join(dir, cleaned)
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return nil, nil, 0, fmt.Errorf("plugin: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	written, err = io.Copy(tmp, content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, 0, fmt.Errorf("plugin: write upload: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, 0, fmt.Errorf("plugin: chmod upload: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, 0, fmt.Errorf("plugin: move upload: %w", err)
	}

	added, removed, err = r.Reload(ctx)
	if err != nil {
		return nil, nil, written, err
	}
	return added, removed, written, nil
}

// RemoveBinary unregisters and removes an external plugin binary. Returns an
// error if the plugin is built-in or not found.
func (r *Registry) RemoveBinary(ctx context.Context, pluginID string) error {
	r.mu.RLock()
	extMgr := r.extManager
	mp, ok := r.plugins[pluginID]
	r.mu.RUnlock()

	if !ok {
		return errdefs.NotFoundf("plugin %q not found", pluginID)
	}
	if mp.p.Info().Builtin {
		return errcode.MethodNotAllowedf("cannot remove built-in plugin %q", pluginID)
	}
	if extMgr == nil || extMgr.PluginDir() == "" {
		return errdefs.Forbiddenf("external plugin directory is not configured")
	}

	var binPath string
	if ep, isExt := mp.p.(*ExternalPlugin); isExt {
		binPath = ep.path
	}
	if binPath == "" {
		binPath = filepath.Join(extMgr.PluginDir(), pluginID)
	}

	_ = mp.p.Shutdown(ctx)

	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("plugin: remove binary %q: %w", binPath, err)
	}

	r.mu.Lock()
	delete(r.plugins, pluginID)
	for nodeType, pid := range r.nodePlugins {
		if pid == pluginID {
			delete(r.nodePlugins, nodeType)
		}
	}
	r.mu.Unlock()

	return nil
}

// CollectNodeSchemas returns UI schemas from all active plugins implementing SchemaProvider.
func (r *Registry) CollectNodeSchemas() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var schemas []map[string]any
	for _, mp := range r.plugins {
		if mp.status != plugin.StatusActive {
			continue
		}
		if sp, ok := mp.p.(plugin.SchemaProvider); ok {
			schemas = append(schemas, sp.NodeSchema())
		}
	}
	return schemas
}

// CollectNodeDefs returns node specs from all active external plugins.
func (r *Registry) CollectNodeDefs() []plugin.NodeSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var specs []plugin.NodeSpec
	for _, mp := range r.plugins {
		if mp.status != plugin.StatusActive {
			continue
		}
		if ep, ok := mp.p.(*ExternalPlugin); ok {
			specs = append(specs, ep.Nodes()...)
		}
	}
	return specs
}

// CollectToolSpecs returns tool specs from all active ToolPlugin instances.
func (r *Registry) CollectToolSpecs() []plugin.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var specs []plugin.ToolSpec
	for _, mp := range r.plugins {
		if mp.status != plugin.StatusActive {
			continue
		}
		if tp, ok := mp.p.(plugin.ToolPlugin); ok {
			specs = append(specs, tp.Tools()...)
		}
	}
	return specs
}

// PluginConfig is the JSON configuration format for plugins.json.
type PluginConfig struct {
	ID      string         `json:"id"`
	Path    string         `json:"path,omitempty"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config,omitempty"`
}

// LoadPluginsJSON loads plugin configurations from a JSON file. Supports
// ${ENV_VAR} expansion in string values.
func LoadPluginsJSON(path string) ([]PluginConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugin: read %q: %w", path, err)
	}

	expanded := os.Expand(string(data), func(key string) string {
		return os.Getenv(key)
	})

	var configs []PluginConfig
	if err := json.Unmarshal([]byte(expanded), &configs); err != nil {
		return nil, fmt.Errorf("plugin: parse %q: %w", path, err)
	}
	return configs, nil
}
