package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

type fakeTool struct{ name string }

func (t fakeTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: t.name}
}

func (t fakeTool) Execute(context.Context, string) (message.Content, error) {
	return message.Content{}, nil
}

// fakeSource is the test double of a plugin source. The call hook is
// optional; without it the source answers direct calls with NotFound,
// which is what the tests assert a missing tool looks like.
type fakeSource struct {
	tools []tool.Tool
	call  func(toolName string, args any) (json.RawMessage, error)
	close func()
}

func (s *fakeSource) Tools() []tool.Tool { return s.tools }

func (s *fakeSource) LazyTools() []tool.LazyTool { return nil }

func (s *fakeSource) Attach(registrar tool.Registrar) {
	for _, candidate := range s.tools {
		_ = registrar.Add(candidate)
	}
}

func (s *fakeSource) Close() error {
	if s.close != nil {
		s.close()
	}
	return nil
}

func (s *fakeSource) CallTool(
	_ context.Context,
	toolName string,
	args any,
) (json.RawMessage, error) {
	if s.call == nil {
		return nil, errdefs.NotFoundf("fake source: tool %q is not provided", toolName)
	}
	return s.call(toolName, args)
}

func writePlugin(t *testing.T, root, dir, manifest string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(path, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestStoreScanEnableKV(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "name": "Hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {"command": "python3", "args": ["server.py"]}
	}`)
	writePlugin(t, root, "bad", `{
		"id": "bad", "version": "0.1.0", "permissions": ["nope"]
	}`)
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	list, err := store.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v, %v; want 2 entries", list, err)
	}
	if list[0].Error == "" && list[1].Error == "" {
		t.Fatal("invalid plugin did not report an error")
	}
	revision := store.Revision()
	notified := 0
	cancel := store.Subscribe(func() { notified++ })
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	cancel()
	if store.Revision() != revision+1 || notified != 1 {
		t.Fatalf("revision=%d notified=%d, want +1/1", store.Revision(), notified)
	}
	enabled, err := store.Enabled()
	if err != nil || len(enabled) != 0 {
		t.Fatalf("Enabled = %v, %v; want none", enabled, err)
	}
	kv, err := store.KV("hello")
	if err != nil {
		t.Fatalf("KV: %v", err)
	}
	if err := kv.Set("counter", "1"); err != nil {
		t.Fatalf("KV.Set: %v", err)
	}
	if value, ok := kv.Get("counter"); !ok || value != "1" {
		t.Fatalf("KV.Get = %q/%v", value, ok)
	}
	dir, err := store.DataDir("hello")
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("data dir: %v", err)
	}
}

func TestManifestLegacyAndRejection(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "old", "version": "0.1.0",
		"entry": "dist/index.js",
		"mcpServers": [{"command": "node", "args": ["server.js"]}]
	}`))
	if err != nil {
		t.Fatalf("ParseManifest legacy: %v", err)
	}
	if err := manifest.Validate(""); err != nil {
		t.Fatalf("Validate legacy: %v", err)
	}
	if manifest.Entry() != "dist/index.js" || len(manifest.Servers()) != 1 {
		t.Fatalf("legacy aliases not normalized: %+v", manifest)
	}
	kraft, err := ParseManifest(context.Background(), []byte(`{
		"id": "old", "version": "0.1.0", "kraft": {"binary": "x"}
	}`))
	if err != nil {
		t.Fatalf("ParseManifest kraft: %v", err)
	}
	if err := kraft.Validate(""); err == nil {
		t.Fatal("kraft field was not rejected")
	}
}

func TestToolSetFanOutAndRemove(t *testing.T) {
	t.Parallel()
	set := NewToolSet()
	source := &fakeSource{tools: []tool.Tool{fakeTool{name: "hello__echo"}}}
	if err := set.AddPlugin("hello", source); err != nil {
		t.Fatalf("AddPlugin: %v", err)
	}
	registry, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	set.Attach(registry)
	if _, ok := registry.Get("hello__echo"); !ok {
		t.Fatal("plugin tool was not attached to the registry")
	}
	if err := set.RemovePlugin(context.Background(), "hello"); err != nil {
		t.Fatalf("RemovePlugin: %v", err)
	}
	if _, ok := registry.Get("hello__echo"); ok {
		t.Fatal("plugin tool still present after RemovePlugin")
	}
	if set.Attached() != 1 {
		t.Fatalf("Attached = %d, want 1", set.Attached())
	}
}
