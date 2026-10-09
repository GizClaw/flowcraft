package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/mcp"
)

// NewSourceFunc builds one plugin's tool source. Tests inject fakes;
// production uses the MCP source factory.
type NewSourceFunc func(
	ctx context.Context,
	entry Entry,
	env map[string]string,
	dataDir string,
) (Source, error)

// HostOptions configures a plugin Host.
type HostOptions struct {
	Store        *Store
	NewSource    NewSourceFunc
	Env          map[string]string
	DrainTimeout time.Duration
}

// Host owns the enabled plugin processes: one tool source per plugin,
// shared by every runtime of a Craft.
type Host struct {
	store        *Store
	newSource    NewSourceFunc
	env          map[string]string
	drainTimeout time.Duration
	tools        *ToolSet

	mu      sync.Mutex
	started bool
	closed  bool
	sources map[string]Source
	envHook func(Entry, map[string]string) error
	stopped func(string)
}

// NewHost opens a plugin host over a store.
func NewHost(opts HostOptions) (*Host, error) {
	if opts.Store == nil {
		return nil, errdefs.Validationf("plugin host: Store is required")
	}
	drain := opts.DrainTimeout
	if drain <= 0 {
		drain = 30 * time.Second
	}
	env := make(map[string]string, len(opts.Env))
	for key, value := range opts.Env {
		env[key] = value
	}
	host := &Host{
		store:        opts.Store,
		newSource:    opts.NewSource,
		env:          env,
		drainTimeout: drain,
		tools:        NewToolSet(),
		sources:      make(map[string]Source),
	}
	if host.newSource == nil {
		host.newSource = host.mcpSource
	}
	return host, nil
}

// Store returns the underlying store.
func (h *Host) Store() *Store { return h.store }

// SetPluginHooks installs the per-plugin env injector and the
// stopped-plugin callback. It must be called before Start.
func (h *Host) SetPluginHooks(
	env func(Entry, map[string]string) error,
	stopped func(string),
) {
	h.mu.Lock()
	h.envHook = env
	h.stopped = stopped
	h.mu.Unlock()
}

// Tools returns the shared tool aggregator.
func (h *Host) Tools() tool.Source { return h.tools }

// ToolSet returns the concrete aggregator.
func (h *Host) ToolSet() *ToolSet { return h.tools }

// Entries returns every valid scanned plugin.
func (h *Host) Entries() ([]Entry, error) { return h.store.Entries() }

// CallTool invokes one tool on a running plugin's primary MCP server.
func (h *Host) CallTool(
	ctx context.Context,
	pluginID, toolName string,
	args any,
) (json.RawMessage, error) {
	h.mu.Lock()
	source := h.sources[pluginID]
	h.mu.Unlock()
	if source == nil {
		return nil, errdefs.NotAvailablef(
			"plugin host: plugin %q is not running", pluginID)
	}
	caller, ok := source.(Caller)
	if !ok {
		return nil, errdefs.NotAvailablef(
			"plugin host: plugin %q does not support direct calls", pluginID)
	}
	return caller.CallTool(ctx, toolName, args)
}

// Revision delegates to the store.
func (h *Host) Revision() uint64 { return h.store.Revision() }

// Subscribe delegates to the store.
func (h *Host) Subscribe(fn func()) func() { return h.store.Subscribe(fn) }

// List returns the UI-facing plugin summaries.
func (h *Host) List() ([]Summary, error) { return h.store.List() }

// KV returns the plugin's namespaced key/value store.
func (h *Host) KV(id string) (*KV, error) { return h.store.KV(id) }

// Start launches every enabled plugin. Individual plugin failures are
// aggregated; the remaining plugins still start.
func (h *Host) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errdefs.NotAvailablef("plugin host: closed")
	}
	h.started = true
	h.mu.Unlock()
	entries, err := h.store.Enabled()
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := h.startEntry(ctx, entry); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", entry.ID, err))
		}
	}
	return errors.Join(errs...)
}

// SetEnabled starts or stops one plugin and bumps the store revision.
func (h *Host) SetEnabled(
	ctx context.Context,
	id string,
	enabled bool,
) error {
	entry, ok := h.store.Entry(id)
	if !ok {
		return errdefs.NotFoundf("plugin host: plugin %q not found", id)
	}
	if enabled {
		if err := h.startEntry(ctx, entry); err != nil {
			return err
		}
	} else if err := h.stopEntry(ctx, id); err != nil {
		return err
	}
	return h.store.SetEnabled(id, enabled)
}

// SkillRoots returns the absolute skill directories of every enabled
// plugin that declares skills:provide.
func (h *Host) SkillRoots() []string {
	entries, err := h.store.Enabled()
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.Manifest.HasPermission("skills:provide") {
			continue
		}
		for _, rel := range entry.Manifest.Skills {
			if path, err := ResolvePath(entry.Dir, rel); err == nil {
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// HookSources returns the hook files of every enabled plugin that
// declares hooks:provide, each anchored to its plugin directory and
// marked untrusted. Both facts matter to a runner: a plugin's hooks.json
// expects to run inside its own directory, and it is third-party
// content, so the runner strips the content-bearing payload fields
// before those commands see them. Sources are ordered by path, so hook
// execution order is stable across calls.
func (h *Host) HookSources() []hooks.ExtraSource {
	entries, err := h.store.Enabled()
	if err != nil {
		return nil
	}
	var out []hooks.ExtraSource
	for _, entry := range entries {
		if !entry.Manifest.HasPermission("hooks:provide") {
			continue
		}
		for _, rel := range entry.Manifest.Hooks {
			if path, err := ResolvePath(entry.Dir, rel); err == nil {
				out = append(out, hooks.ExtraSource{
					Path: path,
					Dir:  entry.Dir,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Close stops every running plugin.
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	ids := make([]string, 0, len(h.sources))
	for id := range h.sources {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), h.drainTimeout)
		if err := h.stopEntry(ctx, id); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	if err := h.tools.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h *Host) startEntry(ctx context.Context, entry Entry) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errdefs.NotAvailablef("plugin host: closed")
	}
	if _, running := h.sources[entry.ID]; running {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	source, err := h.openSource(ctx, entry)
	if err != nil {
		return err
	}
	if err := h.tools.AddPlugin(entry.ID, source); err != nil {
		_ = source.Close()
		return err
	}
	h.mu.Lock()
	h.sources[entry.ID] = source
	h.mu.Unlock()
	return nil
}

// openSource builds the tool source of one plugin. The mcp section is
// honored only when the manifest declares mcp:provide: without the
// grant the whole section is ignored, so no child process is started
// and no host token is minted for it. Direct callers — plugin graph
// nodes — get the refusal as their error instead of a missing-server
// error that would hide the cause.
func (h *Host) openSource(ctx context.Context, entry Entry) (Source, error) {
	dataDir, err := h.store.DataDir(entry.ID)
	if err != nil {
		return nil, err
	}
	if len(entry.Manifest.Servers()) > 0 &&
		!entry.Manifest.HasPermission("mcp:provide") {
		return refusedSource{id: entry.ID}, nil
	}
	h.mu.Lock()
	env := make(map[string]string, len(h.env)+2)
	for key, value := range h.env {
		env[key] = value
	}
	envHook := h.envHook
	h.mu.Unlock()
	if envHook != nil {
		if err := envHook(entry, env); err != nil {
			return nil, err
		}
	}
	return h.newSource(ctx, entry, env, dataDir)
}

// refusedSource stands in for a plugin whose mcp section was ignored
// for lack of the mcp:provide grant. It publishes nothing, and a
// plugin node bound to it fails with the missing grant rather than
// with an empty tool result.
type refusedSource struct{ id string }

func (r refusedSource) Tools() []tool.Tool         { return nil }
func (r refusedSource) LazyTools() []tool.LazyTool { return nil }
func (r refusedSource) Attach(tool.Registrar)      {}
func (r refusedSource) Close() error               { return nil }

func (r refusedSource) CallTool(
	context.Context,
	string,
	any,
) (json.RawMessage, error) {
	return nil, errdefs.Forbiddenf(
		"plugin %s: the mcp section is ignored without the mcp:provide permission",
		r.id)
}

func (h *Host) stopEntry(ctx context.Context, id string) error {
	h.mu.Lock()
	_, running := h.sources[id]
	delete(h.sources, id)
	stopped := h.stopped
	h.mu.Unlock()
	if !running {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, h.drainTimeout)
	defer cancel()
	err := h.tools.RemovePlugin(drainCtx, id)
	if stopped != nil {
		stopped(id)
	}
	return err
}

// mcpSource is the production source factory: one MCP source per
// plugin, with one server entry per declared MCP server.
func (h *Host) mcpSource(
	ctx context.Context,
	entry Entry,
	env map[string]string,
	dataDir string,
) (Source, error) {
	source := mcp.NewSource()
	servers := entry.Manifest.Servers()
	for i, server := range servers {
		name := entry.ID
		if i > 0 {
			name = fmt.Sprintf("%s-%d", entry.ID, i+1)
		}
		transport, err := buildTransport(entry, server, env, dataDir)
		if err != nil {
			_ = source.Close()
			return nil, err
		}
		options := []mcp.ServerOption{mcp.WithPrefix(ToolPrefix(entry.ID))}
		if err := source.AddServer(ctx, name, transport, options...); err != nil {
			_ = source.Close()
			return nil, err
		}
	}
	return &mcpChild{source: source, server: entry.ID}, nil
}

// mcpChild adapts one mcp.Source to the plugin Source contract and
// exposes direct tool calls against the plugin's primary server.
type mcpChild struct {
	source *mcp.Source
	server string
}

func (c *mcpChild) Tools() []tool.Tool         { return c.source.Tools() }
func (c *mcpChild) LazyTools() []tool.LazyTool { return c.source.LazyTools() }
func (c *mcpChild) Attach(r tool.Registrar)    { c.source.Attach(r) }
func (c *mcpChild) Close() error               { return c.source.Close() }

func (c *mcpChild) CallTool(
	ctx context.Context,
	toolName string,
	args any,
) (json.RawMessage, error) {
	return c.source.CallTool(ctx, c.server, toolName, args)
}

func buildTransport(
	entry Entry,
	server MCPServer,
	hostEnv map[string]string,
	dataDir string,
) (mcpsdk.Transport, error) {
	transport := strings.TrimSpace(server.Transport)
	if transport == "" {
		if server.URL != "" {
			transport = "http"
		} else {
			transport = "stdio"
		}
	}
	switch transport {
	case "stdio":
		command, args, err := ResolveCommand(
			entry.Dir, server.Command, server.Args)
		if err != nil {
			return nil, err
		}
		env := make(map[string]string, len(server.Env)+len(hostEnv)+2)
		for key, value := range server.Env {
			env[key] = value
		}
		for key, value := range hostEnv {
			env[key] = value
		}
		env["CRAFT_PLUGIN_ID"] = entry.ID
		env["CRAFT_PLUGIN_DATA_DIR"] = dataDir
		return mcp.Stdio(command, args, env)
	case "http":
		return mcp.StreamableHTTP(server.URL, server.Headers, nil)
	default:
		return nil, errdefs.Validationf(
			"plugin %s: unknown mcp transport %q", entry.ID, transport)
	}
}
