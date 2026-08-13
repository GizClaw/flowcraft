package middleware_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	toolmiddleware "github.com/GizClaw/flowcraft/core/tool/middleware"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

func TestAssemblyFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := toolmiddleware.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup(tool.AssemblyKind, toolmiddleware.AssemblyImpl)
	if !ok {
		t.Fatal("tool.Assembly/middleware factory not registered")
	}

	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{
			"middlewares": {"timeout": {"default": "5ms"}, "recover": {"enabled": true}},
			"dynamic": {"default": "deferred"}
		}`),
		Deps: map[string]any{
			"tool": tooltest.Source(slowTool("slow", 50*time.Millisecond)),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly, ok := value.(*tool.Assembly)
	if !ok {
		t.Fatalf("New returned %T, want *tool.Assembly", value)
	}
	res := assembly.Execute(context.Background(),
		message.ToolCall{ID: "c", Name: "slow", Arguments: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Fatalf("timeout middleware not applied: %+v", res)
	}
	if _, ok := assembly.Catalog().Get(tool.ToolName); !ok {
		t.Fatal("dynamic tool_search not registered")
	}
}

func TestAssemblyFactoryRejectsBadTimeout(t *testing.T) {
	_, err := (toolmiddleware.AssemblyFactory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"middlewares": {"timeout": {"default": "not-a-duration"}}}`),
		Deps: map[string]any{
			"tool": tooltest.Source(slowTool("a", 0)),
		},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("bad timeout = %v, want Validation", err)
	}
}

func TestAssemblyFactoryRequiresSources(t *testing.T) {
	_, err := (toolmiddleware.AssemblyFactory{}).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without sources = %v, want Validation", err)
	}
}

func slowTool(name string, d time.Duration) tool.Tool {
	return tooltest.FuncTool(
		name,
		"",
		func(ctx context.Context, _ string) (string, error) {
			select {
			case <-time.After(d):
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	)
}
