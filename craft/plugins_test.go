package craft

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

type mountTool struct{ name string }

func (t mountTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: t.name}
}

func (t mountTool) Execute(context.Context, string) (message.Content, error) {
	return message.Content{}, nil
}

type mountSource struct{ tools []tool.Tool }

func (s *mountSource) Tools() []tool.Tool { return s.tools }

func (s *mountSource) LazyTools() []tool.LazyTool { return nil }

func (s *mountSource) Attach(registrar tool.Registrar) {
	for _, candidate := range s.tools {
		_ = registrar.Add(candidate)
	}
}

func (s *mountSource) Close() error { return nil }

type fakePluginHost struct {
	set      *plugin.ToolSet
	src      *mountSource
	startErr error
	// entered and release, when set, park Start until the test releases
	// it, so a Close can be interleaved with a start still in flight.
	entered chan struct{}
	release chan struct{}
}

func (h *fakePluginHost) Start(context.Context) error {
	if h.entered != nil {
		close(h.entered)
		<-h.release
	}
	if h.startErr != nil {
		return h.startErr
	}
	return h.set.AddPlugin("hello", h.src)
}

func (h *fakePluginHost) Close() error            { return h.set.Close() }
func (h *fakePluginHost) Revision() uint64        { return 1 }
func (h *fakePluginHost) Subscribe(func()) func() { return func() {} }
func (h *fakePluginHost) Tools() tool.Source      { return h.set }
func (h *fakePluginHost) SkillRoots() []string    { return nil }
func (h *fakePluginHost) HookSources() []hooks.ExtraSource {
	return nil
}
func (h *fakePluginHost) Entries() ([]plugin.Entry, error) {
	return nil, nil
}
func (h *fakePluginHost) CallTool(
	context.Context, string, string, any,
) (json.RawMessage, error) {
	return json.RawMessage("{}"), nil
}

type coreToolsCapability struct{}

func (coreToolsCapability) Name() string { return "tools" }

func (coreToolsCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return tool.Register(registry)
}

func TestCraftMountsPluginTools(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
plugins:
  tool_registry: tools
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
    tools: {kind: tool.Assembly, impl: memory}
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	host := &fakePluginHost{
		set: plugin.NewToolSet(),
		src: &mountSource{tools: []tool.Tool{mountTool{name: "hello__echo"}}},
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{coreToolsCapability{}},
		Plugins:      host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	value, ok := rt.Resource("tools")
	if !ok {
		t.Fatal("tools resource missing")
	}
	assembly, ok := value.(*tool.Assembly)
	if !ok {
		t.Fatalf("tools resource = %T, want *tool.Assembly", value)
	}
	catalog := assembly.Catalog()
	if _, ok := catalog.Get("hello__echo"); !ok {
		t.Fatalf("catalog tools = %v, want hello__echo", catalog.Definitions())
	}
	// A plugin added after the runtime was built must reach the live
	// registry through the attached ToolSet.
	if err := host.set.AddPlugin(
		"late", &mountSource{tools: []tool.Tool{mountTool{name: "late__tool"}}}); err != nil {
		t.Fatalf("AddPlugin late: %v", err)
	}
	if _, ok := catalog.Get("late__tool"); !ok {
		t.Fatalf("live catalog tools = %v, want late__tool", catalog.Definitions())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCraftStartBehindACloseKeepsNothingAlive covers the interleaving a
// close that arrives while Start is bringing the services up creates.
// The plugin host's own close is what stops the processes it knows
// about, and the host refuses to publish one behind that close, so no
// process survives. What the close cannot take back on its own is the
// other half of the start: the plugin watch and the started flag. Both
// are installed under the table lock, so a start that lost to the close
// installs neither — installing them anyway leaves a watcher goroutine
// and a store subscription that outlive the closed Craft and announce a
// start a closed Craft never had.
func TestCraftStartBehindACloseKeepsNothingAlive(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
plugins:
  tool_registry: tools
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
    tools: {kind: tool.Assembly, impl: memory}
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	host := &fakePluginHost{
		set:     plugin.NewToolSet(),
		src:     &mountSource{tools: []tool.Tool{mountTool{name: "hello__echo"}}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{coreToolsCapability{}},
		Plugins:      host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	startErr := make(chan error, 1)
	go func() { startErr <- c.Start(context.Background()) }()
	select {
	case <-host.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the plugin host")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close during the start: %v", err)
	}
	close(host.release)
	if err := <-startErr; !errors.Is(err, ErrCraftClosed) {
		t.Fatalf("Start behind a close = %v, want ErrCraftClosed", err)
	}

	c.mu.Lock()
	started, watch, unsubscribe := c.started, c.watchCancel, c.unsubscribe
	c.mu.Unlock()
	if started {
		t.Fatal("a start that lost to a close marked the Craft started")
	}
	if watch != nil || unsubscribe != nil {
		t.Fatal("a start that lost to a close installed a plugin watch")
	}
	if err := c.Start(context.Background()); !errors.Is(err, ErrCraftClosed) {
		t.Fatalf("Start after the close = %v, want ErrCraftClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
}

// runtimeTools returns a runtime generation's tool catalog and the
// registrar that publishes into it.
func runtimeTools(t *testing.T, rt *runtime.Runtime) (tool.Catalog, tool.Registrar) {
	t.Helper()
	value, ok := rt.Resource("tools")
	if !ok {
		t.Fatal("tools resource missing")
	}
	assembly, ok := value.(*tool.Assembly)
	if !ok {
		t.Fatalf("tools resource = %T, want *tool.Assembly", value)
	}
	registrar, ok := assembly.Catalog().(tool.Registrar)
	if !ok {
		t.Fatalf("catalog %T is not a tool.Registrar", assembly.Catalog())
	}
	return assembly.Catalog(), registrar
}

// TestCraftDetachesClosedRuntimeRegistries is the regression test for
// closed generations staying attached to the process-wide plugin
// ToolSet: gen-1's registry stayed in the fan-out set after
// CloseRuntime, so Attached grew with every generation ever opened and
// a retired generation stayed reachable through the host.
func TestCraftDetachesClosedRuntimeRegistries(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
plugins:
  tool_registry: tools
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
    tools: {kind: tool.Assembly, impl: memory}
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	host := &fakePluginHost{
		set: plugin.NewToolSet(),
		src: &mountSource{tools: []tool.Tool{mountTool{name: "hello__echo"}}},
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{coreToolsCapability{}},
		Plugins:      host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first, err := c.OpenRuntime(ctx, RuntimeKey("gen-1"), RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime gen-1: %v", err)
	}
	if got := host.set.Attached(); got != 1 {
		t.Fatalf("attached after gen-1 open = %d, want 1", got)
	}
	_, firstRegistrar := runtimeTools(t, first)

	// Two generations are attached at once, so the detach has to drop
	// the registrar that closed and keep fanning publications out to
	// the one that is still live.
	second, err := c.OpenRuntime(ctx, RuntimeKey("gen-2"), RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime gen-2: %v", err)
	}
	if got := host.set.Attached(); got != 2 {
		t.Fatalf("attached with two generations open = %d, want 2", got)
	}
	secondCatalog, _ := runtimeTools(t, second)

	if err := c.CloseRuntime(ctx, RuntimeKey("gen-1")); err != nil {
		t.Fatalf("CloseRuntime gen-1: %v", err)
	}
	if got := host.set.Attached(); got != 1 {
		t.Fatalf("attached after gen-1 close = %d, want 1: a closed generation stays attached", got)
	}
	if err := host.set.AddPlugin(
		"late", &mountSource{tools: []tool.Tool{mountTool{name: "late__tool"}}}); err != nil {
		t.Fatalf("AddPlugin late: %v", err)
	}
	if _, ok := secondCatalog.Get("late__tool"); !ok {
		t.Fatalf("live catalog tools = %v, want late__tool: the detach dropped the live generation",
			secondCatalog.Definitions())
	}
	if err := firstRegistrar.Add(mountTool{name: "probe"}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("closed registry Add = %v, want NotAvailable", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCraftStartFailureStaysRetryable covers what a failed Start has to
// leave behind: nothing is recorded as started until the host MCP
// endpoint and the plugin host are both up, so the call can be made
// again instead of succeeding while the plugins stay down.
func TestCraftStartFailureStaysRetryable(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(`
craft:
  id: test
  version: 0.1.0
plugins:
  tool_registry: tools
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
    tools: {kind: tool.Assembly, impl: memory}
  runtime:
    event_bus: bus
`))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	host := &fakePluginHost{
		set:      plugin.NewToolSet(),
		src:      &mountSource{tools: []tool.Tool{mountTool{name: "hello__echo"}}},
		startErr: errors.New("plugin host: the local store is unreadable"),
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{coreToolsCapability{}},
		Plugins:      host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err == nil {
		t.Fatal("Start accepted a plugin host that failed to start")
	}
	host.startErr = nil
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start after the failure: %v", err)
	}
	if tools := host.set.Tools(); len(tools) != 1 {
		t.Fatalf("the retry did not start the plugin host: %d tools mounted", len(tools))
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
