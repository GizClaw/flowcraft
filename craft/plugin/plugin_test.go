package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		"permissions": ["tools:provide"],
		"run": {"command": "python3", "args": ["server.py"]}
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

// TestStoreKVFailsClosedOnABrokenFile pins the read half of plugin KV. A
// file that exists and cannot be read is reported; it is not read as
// "this plugin has no values", because the next Set would then write
// that empty table over whatever the user had stored.
func TestStoreKVFailsClosedOnABrokenFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["tools:provide"], "run": {"command": "true"}
	}`)
	writePlugin(t, root, "quiet", `{
		"id": "quiet", "version": "0.1.0"
	}`)
	stateDir := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    stateDir,
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	path := filepath.Join(stateDir, "kv", "hello.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir kv: %v", err)
	}
	corrupt := []byte(`{"counter": "42"`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write kv: %v", err)
	}
	if _, err := store.KV("hello"); err == nil {
		t.Fatal("a truncated KV file read as valid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kv: %v", err)
	}
	if !bytes.Equal(data, corrupt) {
		t.Fatalf("KV file = %q, want the damaged bytes left alone", data)
	}
	// A file that is not there is the fresh-install case and still reads
	// as empty, so a plugin that never stored anything is not an error.
	if _, err := store.KV("quiet"); err != nil {
		t.Fatalf("KV of a plugin with no file: %v", err)
	}
}

func TestManifestLegacyAndRejection(t *testing.T) {
	t.Parallel()
	manifest, err := ParseManifest(context.Background(), []byte(`{
		"id": "old", "version": "0.1.0",
		"ui": {"entry": "dist/index.js"}
	}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := manifest.Validate(""); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if manifest.Entry() != "dist/index.js" {
		t.Fatalf("ui entry not normalized: %+v", manifest)
	}
	for name, raw := range map[string]string{
		"kraft": `{"id": "old", "version": "0.1.0", "kraft": {"binary": "x"}}`,
		"tools": `{"id": "old", "version": "0.1.0", "tools": [{"name": "echo"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			decoded, err := ParseManifest(context.Background(), []byte(raw))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if err := decoded.Validate(""); err == nil ||
				!strings.Contains(err.Error(), "no longer supported") {
				t.Fatalf("Validate = %v, want the removed field to be refused", err)
			}
		})
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

// registrarSource keeps the registrar it was attached with, so a test
// can drive what a live plugin does after it is up: publish a tool
// without the plugin going away, and later withdraw it.
type registrarSource struct {
	registrar tool.Registrar
}

func (s *registrarSource) Tools() []tool.Tool         { return nil }
func (s *registrarSource) LazyTools() []tool.LazyTool { return nil }
func (s *registrarSource) Close() error               { return nil }

func (s *registrarSource) Attach(registrar tool.Registrar) {
	s.registrar = registrar
}

// TestToolSetWithdrawsOnePluginsTool pins the direction of the fan-out
// RemovePlugin does not: a plugin that is still running withdraws a
// single tool, and the withdrawal reaches every registry downstream
// while the plugin's other tools stay. A tool left behind is a call the
// catalog still offers and the plugin no longer answers.
func TestToolSetWithdrawsOnePluginsTool(t *testing.T) {
	t.Parallel()
	set := NewToolSet()
	source := &registrarSource{}
	if err := set.AddPlugin("hello", source); err != nil {
		t.Fatalf("AddPlugin: %v", err)
	}
	registry, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	set.Attach(registry)
	for _, name := range []string{"hello__echo", "hello__keep"} {
		if err := source.registrar.Add(fakeTool{name: name}); err != nil {
			t.Fatalf("Add(%s): %v", name, err)
		}
	}
	source.registrar.Remove("hello__echo")
	if _, ok := registry.Get("hello__echo"); ok {
		t.Fatal("the withdrawn tool is still in the registry")
	}
	if _, ok := registry.Get("hello__keep"); !ok {
		t.Fatal("the withdrawal took the plugin's other tool with it")
	}
	if set.Attached() != 1 {
		t.Fatalf("Attached = %d, want the plugin still tracked", set.Attached())
	}

	// A generation that attaches afterwards is not handed a tool the
	// plugin withdrew before it existed.
	late, err := tool.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	set.Attach(late)
	if _, ok := late.Get("hello__echo"); ok {
		t.Fatal("a registry attached later saw the withdrawn tool")
	}
	if _, ok := late.Get("hello__keep"); !ok {
		t.Fatal("a registry attached later is missing the live tool")
	}
}
