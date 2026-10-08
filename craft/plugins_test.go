package craft

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
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
	set *plugin.ToolSet
	src *mountSource
}

func (h *fakePluginHost) Start(context.Context) error {
	return h.set.AddPlugin("hello", h.src)
}

func (h *fakePluginHost) Close() error            { return h.set.Close() }
func (h *fakePluginHost) Revision() uint64        { return 1 }
func (h *fakePluginHost) Subscribe(func()) func() { return func() {} }
func (h *fakePluginHost) Tools() tool.Source      { return h.set }
func (h *fakePluginHost) SkillRoots() []string    { return nil }
func (h *fakePluginHost) HookFiles() []string     { return nil }
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
