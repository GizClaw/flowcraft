package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// startRealPluginHost scans one plugin whose process is this test binary
// re-executed as a wire client, starts the host, and returns it once the
// plugin's tools are published.
func startRealPluginHost(t *testing.T) *Host {
	t.Helper()
	if os.Getenv(helperEnv) == "1" {
		t.Skip("running as helper")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	writePlugin(t, root, "hello", wireManifest("hello", "tools.provide"))
	writeHelperCommand(t, dir)
	store := newStoreOver(t, root)
	host := startHost(t, store, nil)
	waitForTool(t, host, "hello__echo")
	return host
}

// waitForTool blocks until the named tool is published.
func waitForTool(t *testing.T, host *Host, name string) tool.Tool {
	t.Helper()
	return waitForToolUpTo(t, host, name, 15*time.Second)
}

// waitForToolUpTo blocks until the named tool is published or the
// deadline passes.
func waitForToolUpTo(
	t *testing.T,
	host *Host,
	name string,
	timeout time.Duration,
) tool.Tool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, candidate := range host.ToolSet().Tools() {
			if candidate.Definition().Name == name {
				return candidate
			}
		}
		if time.Now().After(deadline) {
			names := make([]string, 0)
			for _, candidate := range host.ToolSet().Tools() {
				names = append(names, candidate.Definition().Name)
			}
			t.Fatalf("tool %s not published; tools = %v", name, names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHostRunsRealPluginProcess exercises the production path: store
// scan, process spawn, wire handshake, tool publication and a real tool
// call over the socket.
func TestHostRunsRealPluginProcess(t *testing.T) {
	host := startRealPluginHost(t)
	content, err := waitForTool(t, host, "hello__echo").Execute(
		context.Background(), `{"text":"hi"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(content.Text(), "echo:hi") {
		t.Fatalf("tool result = %q, want echo:hi", content.Text())
	}
}

// TestHostCallToolRealPluginProcess covers the direct-call path plugin
// graph nodes use — Host.CallTool → the plugin's wire source → the
// process — which the registry path above bypasses.
func TestHostCallToolRealPluginProcess(t *testing.T) {
	host := startRealPluginHost(t)
	ctx := context.Background()

	// A result that is not JSON arrives as a JSON string, so the caller
	// always sees valid JSON.
	raw, err := host.CallTool(ctx, "hello", "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if string(raw) != `"echo:hi"` {
		t.Fatalf("echo payload = %s, want %s", raw, `"echo:hi"`)
	}

	// The writes contract of a plugin graph node, returned as JSON.
	writes, err := host.CallTool(ctx, "hello", "node_echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool node_echo: %v", err)
	}
	assertWrites(t, writes, "echo:hi")

	// A tool the plugin never declared is a not-found, not an empty
	// result.
	if _, err := host.CallTool(
		ctx, "hello", "structured_echo", nil); !errdefs.IsNotFound(err) {
		t.Fatalf("CallTool on an undeclared tool = %v, want NotFound", err)
	}

	// A plugin that is not running is a direct-call failure, not a
	// panic or an empty result.
	if _, err := host.CallTool(ctx, "missing", "echo", nil); !errdefs.IsNotAvailable(err) {
		t.Fatalf("CallTool on a missing plugin = %v, want NotAvailable", err)
	}
}

// assertWrites checks the node writes contract.
func assertWrites(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var payload struct {
		Writes map[string]any `json:"writes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload %s is not the writes contract: %v", raw, err)
	}
	if payload.Writes["result"] != want {
		t.Fatalf("writes = %#v, want result=%s", payload.Writes, want)
	}
}
