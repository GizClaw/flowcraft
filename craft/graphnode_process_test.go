package craft

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// TestCraftPluginNodeServerHelper is the re-executed stdio MCP server
// behind TestPluginNodeRunsInRealProcess. It answers the node call the
// way a plugin does: it reads {node, config} and returns {writes}.
func TestCraftPluginNodeServerHelper(t *testing.T) {
	if os.Getenv("CRAFT_NODE_HELPER") != "1" {
		t.Skip("plugin helper")
	}
	// A caller that wants to observe the launch itself names a file to
	// write. The gated lane asserts the file never appears.
	if marker := os.Getenv("CRAFT_NODE_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name: "hello", Version: "0.1.0",
	}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "node_echo",
		InputSchema: map[string]any{"type": "object"},
	}, func(
		_ context.Context,
		req *mcpsdk.CallToolRequest,
	) (*mcpsdk.CallToolResult, error) {
		var payload struct {
			Node struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Graph string `json:"graph"`
			} `json:"node"`
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &payload); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(map[string]any{
			"writes": map[string]any{
				"result":   "echo:" + payload.Node.ID,
				"nodeType": payload.Node.Type,
				"greeting": payload.Config["greeting"],
			},
		})
		if err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(encoded)}},
		}, nil
	})
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		t.Fatalf("helper server: %v", err)
	}
}

// TestPluginNodeRunsInRealProcess is the end-to-end path with nothing
// faked: a craft definition mounting plugin nodes, a plugin manifest
// scanned from disk, a real stdio MCP server process, the synthesized
// graph.NodeType, its handler, and the board writes it produces.
func TestPluginNodeRunsInRealProcess(t *testing.T) {
	if os.Getenv("CRAFT_NODE_HELPER") == "1" {
		t.Skip("running as helper")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	manifest := `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide", "nodes:provide"],
		"nodes": [{"type": "echo", "tool": "node_echo", "timeout": "10s"}],
		"mcp": {
			"command": "./server",
			"args": ["-test.run=TestCraftPluginNodeServerHelper"],
			"env": {"CRAFT_NODE_HELPER": "1"}
		}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := plugin.NewHost(plugin.HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	ctx := context.Background()
	c := newTestCraft(t, nodeDefinition("bot"), host)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPluginTool(t, host, "hello__node_echo")

	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	value, ok := rt.Resource("plugin.hello.node.echo")
	if !ok {
		t.Fatal("synthesized node type resource missing")
	}
	registrar, ok := value.(pluginNodeRegistrar)
	if !ok {
		t.Fatalf("resource value = %T, want a node type registrar", value)
	}

	board := agent.NewBoard()
	if err := registrar.node.Handler(
		coregraph.ExecutionContext{
			Context:  ctx,
			NodeID:   "step-1",
			NodeType: "hello.echo",
			GraphID:  "graph-1",
		},
		board,
		map[string]any{"greeting": "hi"},
	); err != nil {
		t.Fatalf("handler: %v", err)
	}
	vars := board.Vars()
	if vars["result"] != "echo:step-1" {
		t.Fatalf("board result = %#v, want echo:step-1", vars["result"])
	}
	if vars["nodeType"] != "hello.echo" {
		t.Fatalf("board nodeType = %#v, want hello.echo", vars["nodeType"])
	}
	if vars["greeting"] != "hi" {
		t.Fatalf("board greeting = %#v, want the node config", vars["greeting"])
	}
}

// waitForPluginTool blocks until the plugin's tool is published, so the
// node handler runs against a connected source.
func waitForPluginTool(t *testing.T, host *plugin.Host, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, candidate := range host.ToolSet().Tools() {
			if candidate.Definition().Name == name {
				return
			}
		}
		if time.Now().After(deadline) {
			names := make([]string, 0)
			for _, candidate := range host.ToolSet().Tools() {
				names = append(names, candidate.Definition().Name)
			}
			t.Fatalf("plugin tool %s not published; tools = %v", name, names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPluginNodeWithoutMCPGrant pins the consequence of the strict mcp
// gate: the nodes section is gated by nodes:provide alone, so the node
// type is still synthesized — but the plugin behind it has no server,
// because its mcp section was ignored. The plugin process is never
// started, and the node fails with the missing grant instead of hanging
// or writing nothing.
func TestPluginNodeWithoutMCPGrant(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	// The server is the same helper as above, told to record its own
	// launch: what this lane guards against is a started process, and
	// from the outside an ignored section and a broken manifest look
	// alike.
	marker := filepath.Join(dir, "started")
	manifest := `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["nodes:provide"],
		"nodes": [{"type": "echo", "tool": "node_echo", "timeout": "10s"}],
		"mcp": {
			"command": "./server",
			"args": ["-test.run=TestCraftPluginNodeServerHelper"],
			"env": {
				"CRAFT_NODE_HELPER": "1",
				"CRAFT_NODE_MARKER": "` + marker + `"
			}
		}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := plugin.NewHost(plugin.HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	ctx := context.Background()
	c := newTestCraft(t, nodeDefinition("bot"), host)
	started := time.Now()
	for {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("the plugin process started without mcp:provide, after %s",
				time.Since(started))
		}
		// A launched process records itself in milliseconds, so waiting
		// is what makes "never started" observable at all; the loop
		// exits early when the gate leaks.
		if time.Since(started) >= time.Second {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("plugin tools = %v, want none without mcp:provide", tools)
	}

	rt, err := c.OpenRuntime(ctx, DefaultKey, RuntimeOptions{})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	value, ok := rt.Resource("plugin.hello.node.echo")
	if !ok {
		t.Fatal("node type missing: nodes:provide alone should synthesize it")
	}
	registrar, ok := value.(pluginNodeRegistrar)
	if !ok {
		t.Fatalf("resource value = %T, want a node type registrar", value)
	}
	err = registrar.node.Handler(
		coregraph.ExecutionContext{
			Context:  ctx,
			NodeID:   "step-1",
			NodeType: "hello.echo",
			GraphID:  "graph-1",
		},
		agent.NewBoard(),
		nil,
	)
	if !errdefs.IsForbidden(err) {
		t.Fatalf("node handler = %v, want Forbidden", err)
	}
	if !strings.Contains(err.Error(), "mcp:provide") {
		t.Fatalf("node error = %v, want it to name the missing permission", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the plugin server ran during the node call (stat: %v)", err)
	}
}
