package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("expected 0/0, got %d/%d", len(added), len(removed))
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
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed (builtin protected), got %d", len(removed))
	}
	if len(added) != 0 {
		t.Fatalf("expected 0 added, got %d", len(added))
	}
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
	if len(removed) != 1 || removed[0] != "ext-1" {
		t.Fatalf("expected [ext-1] removed, got %v", removed)
	}
	if len(added) != 0 {
		t.Fatalf("expected 0 added, got %d", len(added))
	}
	if _, ok := r.Get("ext-1"); ok {
		t.Fatal("removed plugin should not exist after reload")
	}
}

func TestRegistry_InstallBinary_NoExtManager(t *testing.T) {
	r := NewRegistry()
	_, _, _, err := r.InstallBinary(context.Background(), "x.bin", strings.NewReader("payload"))
	if err == nil {
		t.Fatal("expected error when external manager is not configured")
	}
}

func TestRegistry_InstallBinary_RejectsBadNames(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))

	cases := []string{"", " ", "..", ".", ".hidden", "sub/bin", `windows\path`}
	for _, name := range cases {
		_, _, _, err := r.InstallBinary(context.Background(), name, strings.NewReader("x"))
		if err == nil {
			t.Fatalf("expected rejection for %q", name)
		}
	}
}

func TestRegistry_InstallBinary_WritesAndReloads(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))

	// We pass a non-executable payload. Discover only picks up files with
	// executable bits; InstallBinary chmods to 0755, so Discover will see it
	// but Reload will try to start the (fake) binary and fail. The file must
	// at least land on disk at the expected path.
	_, _, size, err := r.InstallBinary(context.Background(), "fake.bin", strings.NewReader("hello"))
	if err == nil {
		// It's OK for Reload to succeed with 0 adds (init will fail silently).
	}
	// Regardless of reload outcome, the binary must have been written.
	info, statErr := os.Stat(filepath.Join(dir, "fake.bin"))
	if statErr != nil {
		t.Fatalf("binary not written: %v", statErr)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("binary is not executable")
	}
	if size != 5 {
		t.Fatalf("expected 5 bytes, got %d", size)
	}
}

func TestRegistry_RemoveBinary_Builtin(t *testing.T) {
	r := NewRegistry()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: t.TempDir()}))
	p := &testPlugin{info: plugin.PluginInfo{ID: "b", Name: "B", Builtin: true}}
	_ = r.Register(p)

	if err := r.RemoveBinary(context.Background(), "b"); err == nil {
		t.Fatal("expected error removing built-in plugin")
	}
}

func TestRegistry_RemoveBinary_NotFound(t *testing.T) {
	r := NewRegistry()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: t.TempDir()}))
	if err := r.RemoveBinary(context.Background(), "missing"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestRegistry_InstallBinary_CreatesMissingDir(t *testing.T) {
	r := NewRegistry()
	dir := filepath.Join(t.TempDir(), "nested", "plugins")
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))

	_, _, _, _ = r.InstallBinary(context.Background(), "fake", strings.NewReader("x"))

	if _, err := os.Stat(filepath.Join(dir, "fake")); err != nil {
		t.Fatalf("file not present in auto-created dir: %v", err)
	}
}

func TestRegistry_InstallBinary_OverwritesAtomically(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))

	_, _, _, _ = r.InstallBinary(context.Background(), "echo", strings.NewReader("v1"))
	_, _, _, _ = r.InstallBinary(context.Background(), "echo", strings.NewReader("v2-longer"))

	got, err := os.ReadFile(filepath.Join(dir, "echo"))
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	if string(got) != "v2-longer" {
		t.Fatalf("expected v2-longer, got %q", string(got))
	}

	// No leftover .upload-* temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestRegistry_RemoveBinary_RemovesFile(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))

	binPath := filepath.Join(dir, "tool")
	if err := os.WriteFile(binPath, []byte("noop"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &testPlugin{info: plugin.PluginInfo{ID: "tool", Name: "Tool", Builtin: false}}
	_ = r.Register(p)

	if err := r.RemoveBinary(context.Background(), "tool"); err != nil {
		t.Fatalf("RemoveBinary: %v", err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatalf("binary should be deleted, stat err=%v", err)
	}
	if _, ok := r.Get("tool"); ok {
		t.Fatal("plugin should be unregistered after RemoveBinary")
	}
}

func TestRegistry_RemoveBinary_GracefulIfBinaryAlreadyGone(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()
	r.SetExternalManager(NewExternalManager(ExternalManagerConfig{PluginDir: dir}))
	// Plugin registered but no binary on disk: delete should still succeed
	// (idempotent unregister) instead of erroring out.
	p := &testPlugin{info: plugin.PluginInfo{ID: "tool", Name: "Tool", Builtin: false}}
	_ = r.Register(p)

	if err := r.RemoveBinary(context.Background(), "tool"); err != nil {
		t.Fatalf("RemoveBinary should be idempotent: %v", err)
	}
	if _, ok := r.Get("tool"); ok {
		t.Fatal("plugin should be unregistered")
	}
}
