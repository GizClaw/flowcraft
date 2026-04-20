//go:build integration

package pluginhost_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/pluginhost"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func buildEchoPlugin(t *testing.T, outputDir string) string {
	t.Helper()

	pluginBin := filepath.Join(outputDir, "echo")
	buildCmd := exec.Command("go", "build", "-o", pluginBin, "./testdata/echo-plugin")
	buildCmd.Dir = filepath.Join("..")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build echo-plugin: %v\n%s", err, out)
	}
	return pluginBin
}

func TestIntegration_EchoPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pluginDir := t.TempDir()
	buildEchoPlugin(t, pluginDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: 60 * time.Second,
		MaxFailures:    3,
	})

	reg := pluginhost.NewRegistry()
	reg.SetExternalManager(extMgr)

	if err := reg.InitializeAll(ctx); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}
	defer func() {
		reg.ShutdownAll(context.Background())
		extMgr.Stop(context.Background())
	}()

	// Verify plugin is registered and active
	list := reg.List()
	found := false
	for _, p := range list {
		if p.Info.ID == "echo" {
			found = true
			if p.Status != plugin.StatusActive {
				t.Fatalf("echo plugin not active: %v", p.Status)
			}
		}
	}
	if !found {
		t.Fatal("echo plugin not found in registry")
	}

	p, ok := reg.Get("echo")
	if !ok {
		t.Fatal("Get(echo) failed")
	}
	ep, ok := p.(*pluginhost.ExternalPlugin)
	if !ok {
		t.Fatal("not an ExternalPlugin")
	}

	// Verify tool list
	tools := ep.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools from echo plugin")
	}
	if tools[0].Name != "echo" {
		t.Fatalf("expected tool 'echo', got %q", tools[0].Name)
	}

	// Verify node list
	nodes := ep.Nodes()
	if len(nodes) == 0 {
		t.Fatal("no nodes from echo plugin")
	}
	if nodes[0].Type != "echo_node" {
		t.Fatalf("expected node 'echo_node', got %q", nodes[0].Type)
	}

	// Test Tool Execute via gRPC client
	toolClient := ep.ToolClient()
	if toolClient == nil {
		t.Fatal("no tool client")
	}
	result, err := toolClient.Execute(ctx, "echo", `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("tool execute: %v", err)
	}
	if result != `{"message":"hello"}` {
		t.Fatalf("expected echo result, got %q", result)
	}

	// Test Node Execute via ProxyNode
	resolver := func(pluginID string) (plugin.NodeServiceClient, error) {
		rp, rok := reg.Get(pluginID)
		if !rok {
			return nil, fmt.Errorf("plugin %q not found", pluginID)
		}
		rep, rok := rp.(*pluginhost.ExternalPlugin)
		if !rok {
			return nil, fmt.Errorf("plugin %q is not external", pluginID)
		}
		return rep.NodeClient(), nil
	}

	var streamEvents []graph.StreamEvent
	proxyNode := pluginhost.NewProxyNode("test-n1", "echo_node", "echo", nil, resolver, nil)
	board := graph.NewBoard()
	board.SetVar("input", "world")

	execCtx := graph.ExecutionContext{
		Context: ctx,
		Stream: func(ev graph.StreamEvent) {
			streamEvents = append(streamEvents, ev)
		},
	}
	if err := proxyNode.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	output, ok := board.GetVar("output")
	if !ok || output != "world" {
		t.Fatalf("expected output='world', got %v (ok=%v)", output, ok)
	}

	if len(streamEvents) == 0 {
		t.Fatal("expected at least one stream event from StreamEmit")
	}
	if streamEvents[0].Payload != "processing: world" {
		t.Fatalf("unexpected stream event payload: %v", streamEvents[0].Payload)
	}

	// Test Schema injection
	schemaReg := node.NewSchemaRegistry()
	pluginhost.InjectSchemas(reg, schemaReg)

	// Test Tool injection
	toolReg := tool.NewRegistry()
	pluginhost.InjectTools(reg, toolReg)
	if _, ok := toolReg.Get("echo"); !ok {
		t.Fatal("echo tool not injected into tool registry")
	}

	added, removed, err := reg.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("expected no changes on reload, got added=%v removed=%v", added, removed)
	}
}

func TestIntegration_EchoPlugin_WithHostCallbacks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pluginDir := t.TempDir()
	buildEchoPlugin(t, pluginDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: 60 * time.Second,
		MaxFailures:    3,
	})
	reg := pluginhost.NewRegistry()
	reg.SetExternalManager(extMgr)
	if err := reg.InitializeAll(ctx); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}
	defer func() {
		reg.ShutdownAll(context.Background())
		extMgr.Stop(context.Background())
	}()

	resolver := func(pluginID string) (plugin.NodeServiceClient, error) {
		rp, rok := reg.Get(pluginID)
		if !rok {
			return nil, fmt.Errorf("plugin %q not found", pluginID)
		}
		rep := rp.(*pluginhost.ExternalPlugin)
		return rep.NodeClient(), nil
	}

	host := &pluginhost.HostCallbackProvider{
		LLMGenerate: func(_ context.Context, prompt string) (string, error) {
			return "llm-response:" + prompt, nil
		},
		ToolExecute: func(_ context.Context, name, args string) (string, error) {
			return "tool-ok", nil
		},
		SandboxExec: func(_ context.Context, command string) (string, error) {
			return "sandbox-ok", nil
		},
		Signal: func(_ context.Context, _ string, _ any) error {
			return nil
		},
	}

	proxyNode := pluginhost.NewProxyNode("test-cb", "echo_node", "echo", nil, resolver, host)
	board := graph.NewBoard()
	board.SetVar("input", "callback-test")

	execCtx := graph.ExecutionContext{Context: ctx}
	if err := proxyNode.ExecuteBoard(execCtx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	output, ok := board.GetVar("output")
	if !ok || output != "callback-test" {
		t.Fatalf("expected output='callback-test', got %v", output)
	}
}

func TestIntegration_EchoPlugin_ShutdownCleansSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pluginDir := t.TempDir()
	buildEchoPlugin(t, pluginDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: 60 * time.Second,
		MaxFailures:    3,
	})
	reg := pluginhost.NewRegistry()
	reg.SetExternalManager(extMgr)
	if err := reg.InitializeAll(ctx); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}

	p, ok := reg.Get("echo")
	if !ok {
		t.Fatal("echo plugin not found")
	}
	ep := p.(*pluginhost.ExternalPlugin)
	socketPath := ep.SocketPath()

	if socketPath == "" {
		t.Fatal("socket path should not be empty for a running plugin")
	}

	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("socket file should exist while plugin is running")
	}

	reg.ShutdownAll(context.Background())
	extMgr.Stop(context.Background())

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatal("socket file should be removed after shutdown")
	}
}

func TestIntegration_EchoPlugin_Reload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pluginDir := t.TempDir()
	pluginBin := buildEchoPlugin(t, pluginDir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: 60 * time.Second,
		MaxFailures:    3,
	})
	reg := pluginhost.NewRegistry()
	reg.SetExternalManager(extMgr)
	if err := reg.InitializeAll(ctx); err != nil {
		t.Fatalf("InitializeAll: %v", err)
	}
	defer func() {
		reg.ShutdownAll(context.Background())
		extMgr.Stop(context.Background())
	}()

	if _, ok := reg.Get("echo"); !ok {
		t.Fatal("echo plugin not found after init")
	}

	if err := os.Remove(pluginBin); err != nil {
		t.Fatalf("remove plugin binary: %v", err)
	}

	added, removed, err := reg.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload (remove): %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %v", removed)
	}
	if len(added) != 0 {
		t.Fatalf("expected 0 added, got %v", added)
	}
	if _, ok := reg.Get("echo"); ok {
		t.Fatal("echo plugin should be removed after reload")
	}

	buildEchoPlugin(t, pluginDir)

	added, removed, err = reg.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload (add): %v", err)
	}
	if len(added) != 1 {
		t.Fatalf("expected 1 added, got %v", added)
	}
	if len(removed) != 0 {
		t.Fatalf("expected 0 removed, got %v", removed)
	}
	if _, ok := reg.Get("echo"); !ok {
		t.Fatal("echo plugin should be back after reload")
	}
}
