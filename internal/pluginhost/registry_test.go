package pluginhost

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/plugin"
)

type testPlugin struct {
	info   plugin.PluginInfo
	initFn func(ctx context.Context, cfg map[string]any) error
}

func (p *testPlugin) Info() plugin.PluginInfo { return p.info }
func (p *testPlugin) Initialize(ctx context.Context, cfg map[string]any) error {
	if p.initFn != nil {
		return p.initFn(ctx, cfg)
	}
	return nil
}
func (p *testPlugin) Shutdown(_ context.Context) error { return nil }

type testNodePlugin struct {
	testPlugin
	nodeType string
}

func (p *testNodePlugin) NodeType() string                                    { return p.nodeType }
func (p *testNodePlugin) CreateNode(id string, _ map[string]any) (any, error) { return id, nil }
func (p *testNodePlugin) NodeSchema() map[string]any {
	return map[string]any{"type": p.nodeType}
}

type testToolPlugin struct {
	testPlugin
	tools []plugin.ToolSpec
}

func (p *testToolPlugin) Tools() []plugin.ToolSpec { return p.tools }
func (p *testToolPlugin) ExecuteTool(_ context.Context, name, args string) (string, error) {
	return "ok:" + name, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "p1", Name: "Test", Builtin: true}}

	if err := r.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, ok := r.Get("p1")
	if !ok || got.Info().ID != "p1" {
		t.Fatal("expected plugin p1")
	}

	// Duplicate
	if err := r.Register(p); err == nil {
		t.Fatal("expected error on duplicate register")
	}
}

func TestRegistry_EnableDisable(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "p1", Name: "Test"}}
	_ = r.Register(p)

	ctx := context.Background()
	if err := r.Enable(ctx, "p1", nil); err != nil {
		t.Fatalf("enable: %v", err)
	}

	list := r.List()
	if len(list) != 1 || list[0].Status != plugin.StatusActive {
		t.Fatal("expected plugin to be active")
	}

	if err := r.Disable(ctx, "p1"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	list = r.List()
	if list[0].Status != plugin.StatusInactive {
		t.Fatal("expected plugin to be inactive after disable")
	}
}

func TestRegistry_DisableBuiltin(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "builtin-1", Name: "Built-in", Builtin: true}}
	_ = r.Register(p)
	_ = r.InitializeAll(context.Background())

	if err := r.Disable(context.Background(), "builtin-1"); err == nil {
		t.Fatal("expected error when disabling built-in plugin")
	}
}

func TestRegistry_EnableBuiltin(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "builtin-1", Name: "Built-in", Builtin: true}}
	_ = r.Register(p)

	if err := r.Enable(context.Background(), "builtin-1", nil); err == nil {
		t.Fatal("expected MethodNotAllowed when enabling built-in plugin")
	}
}

func TestRegistry_GetNodePlugin(t *testing.T) {
	r := NewRegistry()
	np := &testNodePlugin{
		testPlugin: testPlugin{info: plugin.PluginInfo{ID: "np1", Name: "Node Plugin"}},
		nodeType:   "custom_node",
	}
	_ = r.Register(np)
	_ = r.Enable(context.Background(), "np1", nil)

	got := r.GetNodePlugin("custom_node")
	if got == nil {
		t.Fatal("expected node plugin for custom_node")
	}
	if got.NodeType() != "custom_node" {
		t.Fatalf("expected 'custom_node', got %q", got.NodeType())
	}

	// Non-existent type
	if r.GetNodePlugin("nonexistent") != nil {
		t.Fatal("expected nil for nonexistent node type")
	}
}

func TestRegistry_CollectNodeSchemas(t *testing.T) {
	r := NewRegistry()
	np := &testNodePlugin{
		testPlugin: testPlugin{info: plugin.PluginInfo{ID: "np1"}},
		nodeType:   "my_node",
	}
	_ = r.Register(np)
	_ = r.Enable(context.Background(), "np1", nil)

	schemas := r.CollectNodeSchemas()
	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
}

func TestRegistry_CollectToolSpecs(t *testing.T) {
	r := NewRegistry()
	tp := &testToolPlugin{
		testPlugin: testPlugin{info: plugin.PluginInfo{ID: "tp1"}},
		tools:      []plugin.ToolSpec{{Name: "my_tool", Description: "test"}},
	}
	_ = r.Register(tp)
	_ = r.Enable(context.Background(), "tp1", nil)

	specs := r.CollectToolSpecs()
	if len(specs) != 1 || specs[0].Name != "my_tool" {
		t.Fatalf("expected my_tool, got %v", specs)
	}
}

func TestRegistry_InitializeAll(t *testing.T) {
	r := NewRegistry()
	initCount := 0
	p := &testPlugin{
		info: plugin.PluginInfo{ID: "p1"},
		initFn: func(_ context.Context, _ map[string]any) error {
			initCount++
			return nil
		},
	}
	_ = r.Register(p)

	_ = r.InitializeAll(context.Background())
	if initCount != 1 {
		t.Fatalf("expected 1 init call, got %d", initCount)
	}

	// Already active → skip
	_ = r.InitializeAll(context.Background())
	if initCount != 1 {
		t.Fatalf("expected still 1 init call, got %d", initCount)
	}
}

func TestRegistry_ShutdownAll(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "p1"}}
	_ = r.Register(p)
	_ = r.Enable(context.Background(), "p1", nil)

	r.ShutdownAll(context.Background())

	list := r.List()
	if list[0].Status != plugin.StatusInactive {
		t.Fatal("expected inactive after ShutdownAll")
	}
}

func TestRegistry_UpdateConfig(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "ext1"}}
	_ = r.Register(p)
	_ = r.Enable(context.Background(), "ext1", map[string]any{"key": "val1"})

	if err := r.UpdateConfig(context.Background(), "ext1", map[string]any{"key": "val2"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	list := r.List()
	if list[0].Config["key"] != "val2" {
		t.Fatalf("expected 'val2', got %v", list[0].Config["key"])
	}
}

func TestRegistry_UpdateConfigBuiltin(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "b1", Builtin: true}}
	_ = r.Register(p)

	if err := r.UpdateConfig(context.Background(), "b1", nil); err == nil {
		t.Fatal("expected error when updating built-in config")
	}
}

func TestRegistry_EnableNotFound(t *testing.T) {
	r := NewRegistry()
	if err := r.Enable(context.Background(), "nonexistent", nil); err == nil {
		t.Fatal("expected not found error")
	}
}

func TestRegistry_Reload_NoExtManager(t *testing.T) {
	r := NewRegistry()
	added, removed, err := r.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if added != 0 || removed != 0 {
		t.Fatalf("expected 0/0, got %d/%d", added, removed)
	}
}

func TestRegistry_Reload_BuiltinNotAffected(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{info: plugin.PluginInfo{ID: "builtin-1", Name: "Built-in", Builtin: true}}
	_ = r.Register(p)
	_ = r.InitializeAll(context.Background())

	// Set up an external manager with empty dir so no external plugins exist
	dir := t.TempDir()
	extMgr := NewExternalManager(ExternalManagerConfig{PluginDir: dir})
	r.SetExternalManager(extMgr)

	added, removed, err := r.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// Built-in plugin should not be removed
	if removed != 0 {
		t.Fatalf("expected 0 removed (builtin protected), got %d", removed)
	}
	if added != 0 {
		t.Fatalf("expected 0 added, got %d", added)
	}
	// Verify builtin still exists
	if _, ok := r.Get("builtin-1"); !ok {
		t.Fatal("builtin plugin should still exist after reload")
	}
}

func TestRegistry_Reload_RemoveNonexistent(t *testing.T) {
	r := NewRegistry()
	// Register a non-builtin plugin manually
	p := &testPlugin{info: plugin.PluginInfo{ID: "ext-1", Name: "External"}}
	_ = r.Register(p)
	_ = r.Enable(context.Background(), "ext-1", nil)

	// Empty dir means this plugin's binary doesn't exist
	dir := t.TempDir()
	extMgr := NewExternalManager(ExternalManagerConfig{PluginDir: dir})
	r.SetExternalManager(extMgr)

	added, removed, err := r.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if added != 0 {
		t.Fatalf("expected 0 added, got %d", added)
	}
	if _, ok := r.Get("ext-1"); ok {
		t.Fatal("removed plugin should not exist after reload")
	}
}
