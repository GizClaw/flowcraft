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
			"middlewares": {
				"recover": {"enabled": true},
				"telemetry": {"enabled": true},
				"timeout": {"default": "5ms"}
			},
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
	if !res.IsError || !strings.Contains(res.Content.Text(), "timed out") {
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

func TestAssemblyFactoryAppliesResultLimit(t *testing.T) {
	value, err := (toolmiddleware.AssemblyFactory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"middlewares": {"result_limit": {"max": 40, "marker": "[cut]"}}}`),
		Deps: map[string]any{
			"tool": tooltest.Source(tooltest.FuncTool("long", "",
				func(context.Context, string) (string, error) {
					return strings.Repeat("x", 500), nil
				})),
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
		message.ToolCall{ID: "c", Name: "long", Arguments: []byte(`{}`)})
	if got := len([]rune(res.Content.Text())); got != 40 {
		t.Fatalf("limited text = %d runes, want 40", got)
	}
	if !strings.HasSuffix(res.Content.Text(), "[cut]") {
		t.Fatalf("content = %q, want the configured marker", res.Content.Text())
	}
}

func TestAssemblyFactoryRejectsBadResultLimit(t *testing.T) {
	for name, settings := range map[string]string{
		"non-positive max": `{"middlewares": {"result_limit": {"max": 0}}}`,
		// The settings subtree is strict: a typo fails the build instead
		// of silently dropping the budget.
		"misspelled key": `{"middlewares": {"result_limit": {"max": 100, "part_budget_byte": 1}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (toolmiddleware.AssemblyFactory{}).New(context.Background(), resource.Input{
				Settings: []byte(settings),
				Deps: map[string]any{
					"tool": tooltest.Source(slowTool("a", 0)),
				},
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("settings %s = %v, want Validation", settings, err)
			}
		})
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
