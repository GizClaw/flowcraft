package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

// TestSessionSearchCapsLimitAtPool pins the cap on the model-supplied
// limit: every hit is loaded and takes a discovery-pool slot, so a request
// for a million hits must not churn a pool that holds two.
func TestSessionSearchCapsLimitAtPool(t *testing.T) {
	assembly, err := tool.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			funcTool("tool_a", "1"), funcTool("tool_b", "2"),
			funcTool("tool_c", "3"), funcTool("tool_d", "4"),
		}}},
		tool.WithDynamic(tool.Policy{
			Default:   tool.ExposureDeferred,
			Discovery: tool.DiscoveryPolicy{MaxTools: 2, MaxBytes: 1 << 20},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewSession()
	hits, err := session.Search(context.Background(), "tool", 1_000_000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search(tool, 1000000) = %d hits, want the pool size 2", len(hits))
	}
	// A limit below the cap is left alone.
	hits, err = session.Search(context.Background(), "tool", 1)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search(tool, 1) = %d hits, %v", len(hits), err)
	}
}
