package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// options are the inputs of one anvil run.
type options struct {
	// definition is the path of craft.yaml.
	definition string
	// dataDir is the writable root: plugin state, plugin data, notes
	// files and the per-runtime layer the tour writes.
	dataDir string
	// out receives the tour's narration.
	out io.Writer
	// pluginTimeout bounds the wait for a plugin's MCP server; the first
	// run compiles it. Zero means two minutes.
	pluginTimeout time.Duration
	// reloadTimeout bounds the waits on effects that land on the craft's
	// goroutines. Zero means thirty seconds.
	reloadTimeout time.Duration
}

func (o options) withDefaults() options {
	if o.out == nil {
		o.out = io.Discard
	}
	if o.pluginTimeout <= 0 {
		o.pluginTimeout = 2 * time.Minute
	}
	if o.reloadTimeout <= 0 {
		o.reloadTimeout = 30 * time.Second
	}
	return o
}

// app is one assembled host application: the parsed definition, the
// plugin host, the Craft and the event log the tour reads.
type app struct {
	opts       options
	def        craft.Definition
	definition string // absolute path of craft.yaml
	dataDir    string // absolute writable root

	capability *workshop
	plugins    *plugin.Host
	craft      *craft.Craft
	events     *eventLog
	print      *printer

	calls int
}

// newApp assembles the application the way a shell would: parse the
// definition, open the plugin store, build the plugin host, hand both to
// craft.New together with the compile-time capabilities.
func newApp(opts options) (*app, error) {
	opts = opts.withDefaults()

	definition, err := filepath.Abs(opts.definition)
	if err != nil {
		return nil, fmt.Errorf("resolve definition path: %w", err)
	}
	raw, err := os.ReadFile(definition)
	if err != nil {
		return nil, fmt.Errorf("read definition: %w", err)
	}
	def, err := craft.ParseDefinition(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", definition, err)
	}
	dataDir, err := filepath.Abs(opts.dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	a := &app{
		opts:       opts,
		def:        def,
		definition: definition,
		dataDir:    dataDir,
		print:      newPrinter(opts.out),
	}
	a.events = newEventLog(a.print)

	store, err := plugin.NewStore(plugin.Options{
		Roots:       a.pluginRoots(),
		StateDir:    filepath.Join(dataDir, "plugin-state"),
		DataDirRoot: filepath.Join(dataDir, "plugin-data"),
		HostVersion: craft.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("open plugin store: %w", err)
	}
	host, err := plugin.NewHost(plugin.HostOptions{Store: store})
	if err != nil {
		return nil, fmt.Errorf("open plugin host: %w", err)
	}
	a.plugins = host

	a.capability = newWorkshop()
	c, err := craft.New(def, craft.Options{
		ConfigDir:     dataDir,
		DataDir:       dataDir,
		DefinitionDir: filepath.Dir(definition),
		Capabilities:  []craft.Capability{a.capability},
		Plugins:       host,
	})
	if err != nil {
		return nil, fmt.Errorf("assemble craft: %w", err)
	}
	a.craft = c

	// The emit_event primitive publishes on the craft plane, namespaced
	// by the calling plugin. The emitter can only be wired once the
	// Craft exists, which is necessarily after craft.New.
	a.capability.setEmitter(
		func(ctx context.Context, pluginID, subject string, payload any) error {
			return c.Emit(ctx, event.Subject("plugin."+pluginID+"."+subject), payload)
		})

	// One sink for the whole craft plane: craft subjects plus the plugin
	// namespace Emit uses. Attach before Start so craft.started is seen.
	if _, err := c.Attach(context.Background(), event.Pattern(">"), a.events); err != nil {
		return nil, fmt.Errorf("attach event sink: %w", err)
	}
	return a, nil
}

// pluginRoots resolves plugins.builtin and plugins.roots against the
// definition's directory. The definition names the roots; the
// application decides they are relative to itself.
func (a *app) pluginRoots() []plugin.Root {
	if a.def.Plugins == nil {
		return nil
	}
	dir := filepath.Dir(a.definition)
	resolve := func(root string) string {
		if filepath.IsAbs(root) {
			return root
		}
		return filepath.Join(dir, root)
	}
	var roots []plugin.Root
	if builtin := strings.TrimSpace(a.def.Plugins.Builtin); builtin != "" {
		roots = append(roots, plugin.Root{Path: resolve(builtin), Builtin: true})
	}
	for _, root := range a.def.Plugins.Roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		roots = append(roots, plugin.Root{Path: resolve(root)})
	}
	return roots
}

// resourceNames lists the deploy document's resources, sorted.
func (a *app) resourceNames() []string {
	if a.def.Deploy == nil {
		return nil
	}
	names := make([]string, 0, len(a.def.Deploy.Resources))
	for name := range a.def.Deploy.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pluginToolNames lists what the enabled plugins have published so far.
func (a *app) pluginToolNames() []string {
	candidates := a.plugins.ToolSet().Tools()
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, candidate.Definition().Name)
	}
	sort.Strings(names)
	return names
}

// assembly returns one runtime's current tool assembly. It is read
// again after every reload, because a reload swaps in a new generation
// with new resource values.
func (*app) assembly(rt *runtime.Runtime) (*tool.Assembly, error) {
	value, ok := rt.Resource("tools")
	if !ok {
		return nil, errors.New("the runtime has no tools resource")
	}
	assembly, ok := value.(*tool.Assembly)
	if !ok {
		return nil, fmt.Errorf("the tools resource is %T, want *tool.Assembly", value)
	}
	return assembly, nil
}

// catalog lists the tool names one runtime currently offers.
func (a *app) catalog(rt *runtime.Runtime) ([]string, error) {
	assembly, err := a.assembly(rt)
	if err != nil {
		return nil, err
	}
	definitions := assembly.Catalog().Definitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	return names, nil
}

// describe reads one tool's description in the current generation.
func (a *app) describe(rt *runtime.Runtime, name string) (string, error) {
	assembly, err := a.assembly(rt)
	if err != nil {
		return "", err
	}
	candidate, ok := assembly.Catalog().Get(name)
	if !ok {
		return "", fmt.Errorf("tool %s is not in the catalog", name)
	}
	return candidate.Definition().Description, nil
}

// call runs one tool through the runtime's assembly and prints it.
func (a *app) call(
	ctx context.Context,
	rt *runtime.Runtime,
	name string,
	args map[string]any,
) (string, error) {
	assembly, err := a.assembly(rt)
	if err != nil {
		return "", err
	}
	a.calls++
	call, err := message.NewToolCall(fmt.Sprintf("anvil-%d", a.calls), name, args)
	if err != nil {
		return "", fmt.Errorf("build call: %w", err)
	}
	result := assembly.Execute(ctx, call)
	text := result.Content.Text()
	if result.IsError {
		return "", fmt.Errorf("%s failed: %s", name, text)
	}
	a.print.line("%s %s -> %s", name, compactArguments(call.Arguments), text)
	return text, nil
}

// waitForPluginTool waits until the plugin host has published one tool.
func (a *app) waitForPluginTool(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		for _, candidate := range a.plugins.ToolSet().Tools() {
			if candidate.Definition().Name == name {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"timed out waiting for plugin tool %s; published: %v",
				name, a.pluginToolNames())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitForCatalog waits until one runtime's catalog carries (or drops) a
// tool; plugin changes reach a live runtime through the shared tool set.
func (a *app) waitForCatalog(
	ctx context.Context,
	rt *runtime.Runtime,
	name string,
	want bool,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for {
		names, err := a.catalog(rt)
		if err != nil {
			return err
		}
		if slices.Contains(names, name) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for catalog %s=%v; tools = %v",
				name, want, names)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// showTools prints one runtime's catalog.
func (a *app) showTools(rt *runtime.Runtime) error {
	names, err := a.catalog(rt)
	if err != nil {
		return err
	}
	a.print.line("tools: %s", strings.Join(names, ", "))
	return nil
}

// scratchFiles are the files the tour owns under the data directory.
var scratchFiles = []string{
	"notes-default.txt",
	"notes-alpha.txt",
	"notes-alpha-round2.txt",
	"alpha.layer.json",
}

// resetScratch removes the tour's own files so a previous run cannot
// leak into this one. Everything else in the data directory stays.
func (a *app) resetScratch() (int, error) {
	removed := 0
	for _, name := range scratchFiles {
		err := os.Remove(filepath.Join(a.dataDir, name))
		switch {
		case err == nil:
			removed++
		case errors.Is(err, fs.ErrNotExist):
		default:
			return removed, fmt.Errorf("reset scratch file %s: %w", name, err)
		}
	}
	return removed, nil
}

// writeAlphaLayer writes the per-runtime layer that points the alpha
// runtime's notes at file, and returns the layer's path.
func (a *app) writeAlphaLayer(file string) (string, error) {
	document := map[string]any{
		"resources": map[string]any{
			"notes": map[string]any{
				"kind":     notesKind,
				"impl":     notesImpl,
				"settings": map[string]any{"path": file},
			},
		},
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode layer: %w", err)
	}
	path := filepath.Join(a.dataDir, "alpha.layer.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write layer: %w", err)
	}
	return path, nil
}

// compactArguments renders a tool call's arguments on one line.
func compactArguments(raw json.RawMessage) string {
	compact := compactJSON(raw)
	if compact == "" {
		return "{}"
	}
	return compact
}
