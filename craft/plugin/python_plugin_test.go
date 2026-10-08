package plugin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestHostRunsPythonPlugin covers the language promise of the plugin
// contract: a plugin written in Python — the case the MCP tool path and
// the plugin graph nodes exist for — is scanned, launched over stdio
// and called directly, exactly like the Go helper in process_test.go.
//
// It is opt-in: it needs the uv toolchain and, on a cold cache, network
// access. Set CRAFT_TEST_PYTHON_PLUGIN=1 to run it; without the
// variable it skips, so `make test` stays hermetic.
func TestHostRunsPythonPlugin(t *testing.T) {
	if os.Getenv("CRAFT_TEST_PYTHON_PLUGIN") != "1" {
		t.Skip("set CRAFT_TEST_PYTHON_PLUGIN=1 to run the Python lane")
	}
	if os.Getenv("CRAFT_PLUGIN_HELPER") == "1" {
		t.Skip("running as helper")
	}
	if _, err := exec.LookPath("uvx"); err != nil {
		t.Skipf("uvx is not on PATH: %v", err)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "pynode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script, err := os.ReadFile(pythonFixturePath())
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o700); err != nil {
		t.Fatalf("mkdir server: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "server", "main.py"), script, 0o600); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	manifest, err := json.Marshal(map[string]any{
		"id": "pynode", "version": "0.1.0",
		"permissions": []string{"mcp:provide", "nodes:provide"},
		"nodes": []map[string]any{
			{"type": "echo", "tool": "node_echo", "timeout": "20s"},
		},
		"mcp": map[string]any{
			"command": "uvx",
			// The relative argument is resolved against the plugin
			// root by the host, so the child gets an absolute path
			// even though the command is bare.
			"args": []string{"--from", "mcp[cli]", "python", "server/main.py"},
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), manifest, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	// A cold uv cache resolves and installs the SDK, which takes a
	// while; the deadline is generous on purpose.
	waitForToolUpTo(t, host, "pynode__node_echo", 3*time.Minute)

	payload, err := host.CallTool(
		context.Background(), "pynode", "node_echo", map[string]any{
			"node":   map[string]any{"id": "step-1", "type": "pynode.echo"},
			"config": map[string]any{"greeting": "hi"},
		})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var writes struct {
		Writes map[string]any `json:"writes"`
	}
	if err := json.Unmarshal(payload, &writes); err != nil {
		t.Fatalf("payload %s is not the writes contract: %v", payload, err)
	}
	if writes.Writes["result"] != "echo:step-1" {
		t.Fatalf("writes = %#v, want result=echo:step-1", writes.Writes)
	}
	if writes.Writes["greeting"] != "hi" {
		t.Fatalf("writes = %#v, want the node config forwarded", writes.Writes)
	}
}

// TestStoreScansPythonPluginFixture checks the fixture itself without
// launching Python: the manifest the lane above installs must be valid
// and must declare the node the host mounts.
func TestStoreScansPythonPluginFixture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "pynode")
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script, err := os.ReadFile(pythonFixturePath())
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(script) == 0 {
		t.Fatal("the Python plugin fixture is empty")
	}
	if err := os.WriteFile(
		filepath.Join(dir, "server", "main.py"), script, 0o600); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	manifest, err := json.Marshal(map[string]any{
		"id": "pynode", "version": "0.1.0",
		"permissions": []string{"mcp:provide", "nodes:provide"},
		"nodes": []map[string]any{
			{"type": "echo", "tool": "node_echo", "timeout": "20s"},
		},
		"mcp": map[string]any{
			"command": "uvx",
			"args":    []string{"--from", "mcp[cli]", "python", "server/main.py"},
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), manifest, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	entries, err := store.Enabled()
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Enabled = %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if !entry.Manifest.HasPermission("nodes:provide") {
		t.Fatalf("permissions = %v, want nodes:provide",
			entry.Manifest.Permissions)
	}
	if len(entry.Manifest.Nodes) != 1 ||
		entry.Manifest.Nodes[0].Tool != "node_echo" {
		t.Fatalf("nodes = %#v, want the node_echo declaration",
			entry.Manifest.Nodes)
	}
	command, args, err := ResolveCommand(
		entry.Dir, entry.Manifest.MCP.Command, entry.Manifest.MCP.Args)
	if err != nil {
		t.Fatalf("ResolveCommand: %v", err)
	}
	if command != "uvx" {
		t.Fatalf("command = %q, want uvx", command)
	}
	last := args[len(args)-1]
	if !filepath.IsAbs(last) || filepath.Base(last) != "main.py" {
		t.Fatalf("resolved script argument = %q, want the absolute plugin path", last)
	}
}

// pythonFixturePath is the Python MCP server the lane above installs.
func pythonFixturePath() string {
	return filepath.Join("testdata", "python_plugin", "server", "main.py")
}
