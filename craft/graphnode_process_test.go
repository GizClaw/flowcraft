package craft

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// TestPluginNodeRunsInRealProcess is the whole plugin-node path end to end:
// the manifest is scanned from disk, the plugin process spawns and dials
// craft's socket, its declaration is projected, the synthesized
// graph.NodeType is mounted, and its handler returns the board writes the
// plugin produced.
func TestPluginNodeRunsInRealProcess(t *testing.T) {
	root := t.TempDir()
	writeWirePlugin(t, root, "hello", "0.1.0", nil, "tools:provide", "nodes:provide")
	host := newWirePluginHost(t, root).host
	ctx := context.Background()
	c := newTestCraft(t, nodeDefinition("bot"), host)
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

// TestPluginNodeWithoutToolsGrant pins what the two grants are for. The
// nodes section is gated by nodes:provide alone, so the node type is still
// synthesized — but the plugin runs without tools:provide, so nothing it
// declares is projected, and a node backed by it fails with the missing
// grant instead of writing nothing.
func TestPluginNodeWithoutToolsGrant(t *testing.T) {
	root := t.TempDir()
	writeWirePlugin(t, root, "hello", "0.1.0", nil, "nodes:provide")
	host := newWirePluginHost(t, root).host
	ctx := context.Background()
	c := newTestCraft(t, nodeDefinition("bot"), host)
	waitForRunning(t, host, "hello")

	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("plugin tools = %v, want none without tools:provide", tools)
	}
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
	if err == nil || !strings.Contains(err.Error(), "tools:provide") {
		t.Fatalf("node handler = %v, want the missing tools:provide grant", err)
	}
}

// waitForRunning blocks until the host has a process for one plugin. A
// direct call is how the public surface tells the two apart: a plugin with
// no source reports NotAvailable, while a running one that publishes nothing
// reports the grant it is missing.
func waitForRunning(t *testing.T, host *plugin.Host, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := host.CallTool(context.Background(), id, "node_echo", nil)
		if err == nil || !errdefs.IsNotAvailable(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin %s never started: %v", id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
