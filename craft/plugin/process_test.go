package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// TestPluginServerHelper is the re-executed stdio MCP server used by
// the real-process tests in this file.
//
// It serves three tools that between them cover the result shapes a
// plugin-provided graph node consumes: a text result ("echo"), the
// writes contract as JSON text ("node_echo", what a server following
// the spec's text mirror returns), and the same contract carried only
// in structuredContent ("structured_echo", what a server that omits
// the text mirror returns).
func TestPluginServerHelper(t *testing.T) {
	if os.Getenv("CRAFT_PLUGIN_HELPER") != "1" {
		t.Skip("plugin helper")
	}
	type args struct {
		Text string `json:"text,omitempty"`
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name: "helper", Version: "0.1.0",
	}, nil)
	// The raw handler form is used deliberately: it writes the result
	// verbatim, the way a plugin server written against the protocol
	// (rather than against this SDK's typed helpers, which append a
	// JSON text mirror to structured output) sends it.
	handle := func(
		build func(in args) *mcpsdk.CallToolResult,
	) mcpsdk.ToolHandler {
		return func(
			_ context.Context,
			req *mcpsdk.CallToolRequest,
		) (*mcpsdk.CallToolResult, error) {
			var in args
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
					return nil, err
				}
			}
			return build(in), nil
		}
	}
	add := func(name string, handler mcpsdk.ToolHandler) {
		server.AddTool(&mcpsdk.Tool{
			Name:        name,
			InputSchema: map[string]any{"type": "object"},
		}, handler)
	}
	add("echo", handle(func(in args) *mcpsdk.CallToolResult {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "echo:" + in.Text},
			},
		}
	}))
	add("node_echo", handle(func(in args) *mcpsdk.CallToolResult {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{
					Text: mustJSON(nodeWrites(in.Text)),
				},
			},
		}
	}))
	add("structured_echo", handle(func(in args) *mcpsdk.CallToolResult {
		// No Content: only the structured payload, as a server that
		// skips the spec's text mirror sends it.
		return &mcpsdk.CallToolResult{
			StructuredContent: nodeWrites(in.Text),
		}
	}))
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		t.Fatalf("helper server: %v", err)
	}
}

func nodeWrites(text string) map[string]any {
	return map[string]any{"writes": map[string]any{"result": "echo:" + text}}
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// startRealPluginHost scans one plugin whose stdio server is this test
// binary re-executed, starts the host, and returns it once the plugin's
// tools are published.
func startRealPluginHost(t *testing.T) *Host {
	t.Helper()
	if os.Getenv("CRAFT_PLUGIN_HELPER") == "1" {
		t.Skip("running as helper")
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	manifest := `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {
			"command": "./server",
			"args": ["-test.run=TestPluginServerHelper"],
			"env": {"CRAFT_PLUGIN_HELPER": "1"}
		}
	}`
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
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
// scan, process spawn, MCP handshake, tool publication and a real tool
// call over stdio.
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
// graph nodes use — Host.CallTool → the plugin's child source →
// mcp.Source.CallTool → the plugin process — which the registry path
// above bypasses.
func TestHostCallToolRealPluginProcess(t *testing.T) {
	host := startRealPluginHost(t)
	ctx := context.Background()

	// A plain text result is not JSON, so it arrives as a JSON string.
	raw, err := host.CallTool(ctx, "hello", "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if string(raw) != `"echo:hi"` {
		t.Fatalf("echo payload = %s, want %s", raw, `"echo:hi"`)
	}

	// The writes contract of a plugin graph node, returned as JSON text.
	writes, err := host.CallTool(ctx, "hello", "node_echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool node_echo: %v", err)
	}
	assertWrites(t, writes, "echo:hi")

	// The same contract carried only in structuredContent.
	structured, err := host.CallTool(
		ctx, "hello", "structured_echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool structured_echo: %v", err)
	}
	assertWrites(t, structured, "echo:hi")

	// A plugin that is not running is a direct-call failure, not a
	// panic or an empty result.
	if _, err := host.CallTool(ctx, "missing", "echo", nil); !errdefs.IsNotAvailable(err) {
		t.Fatalf("CallTool on a missing plugin = %v, want NotAvailable", err)
	}
}

func assertWrites(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var decoded struct {
		Writes map[string]any `json:"writes"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload %s is not the writes contract: %v", raw, err)
	}
	if decoded.Writes["result"] != want {
		t.Fatalf("writes = %#v, want result=%q", decoded.Writes, want)
	}
}
