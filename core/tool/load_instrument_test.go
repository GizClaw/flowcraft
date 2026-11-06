package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// TestLazyLoadEmitsSpanOnce pins the miss-path rule: a deferred tool's
// materialisation is one span, and the calls that find it already loaded add
// nothing. A span per call would be noise; no span at all hides a cold start.
func TestLazyLoadEmitsSpanOnce(t *testing.T) {
	recorder := telemetrytest.New(t)

	loads := 0
	lazy := tooltest.LazyTool("deferred", func(context.Context) (tool.Tool, error) {
		loads++
		return tooltest.FuncTool("deferred", "", func(context.Context, string) (string, error) {
			return "ok", nil
		}), nil
	})
	registry, err := tool.NewRegistry([]tool.Source{tooltest.LazySource(lazy)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	proxy, ok := registry.Get("deferred")
	if !ok {
		t.Fatal("lazy tool not registered")
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := proxy.Execute(ctx, `{}`); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}

	if loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
	recorder.RequireCount(t, "tool.load", 1)
	span := recorder.RequireNamed(t, "tool.load")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrToolName, "deferred")
}
