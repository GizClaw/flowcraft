package pluginhost

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/plugin"
)

func TestProxyTool_Definition(t *testing.T) {
	spec := plugin.ToolSpec{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: map[string]any{"type": "object"},
	}
	pt := NewProxyTool(spec, "plugin-1", nil)

	def := pt.Definition()
	if def.Name != "test_tool" {
		t.Fatalf("expected 'test_tool', got %q", def.Name)
	}
	if def.Description != "A test tool" {
		t.Fatalf("expected description, got %q", def.Description)
	}
}

func TestProxyTool_Execute(t *testing.T) {
	spec := plugin.ToolSpec{Name: "echo"}
	executor := func(_ context.Context, name, args string) (string, error) {
		return "result:" + name + ":" + args, nil
	}
	pt := NewProxyTool(spec, "plugin-1", executor)

	result, err := pt.Execute(context.Background(), `{"input":"hello"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != `result:echo:{"input":"hello"}` {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestProxyTool_ExecuteNoExecutor(t *testing.T) {
	pt := NewProxyTool(plugin.ToolSpec{Name: "no_exec"}, "p1", nil)
	_, err := pt.Execute(context.Background(), "{}")
	if err == nil {
		t.Fatal("expected error when no executor configured")
	}
}
