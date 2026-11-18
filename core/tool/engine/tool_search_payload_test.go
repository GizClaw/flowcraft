package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// deferredTools builds a dynamic assembly over n deferred tools whose placeholder
// descriptions the caller sizes. Placeholders are what the search index reads
// before a hit is loaded, so this is the third-party text path.
func deferredTools(t *testing.T, n, descriptionBytes int) *engine.Assembly {
	t.Helper()
	lazy := make([]tool.LazyTool, 0, n)
	for i := range n {
		name := fmt.Sprintf("tool-%02d", i)
		lazy = append(lazy, tool.LazyTool{
			Name: name,
			Placeholder: message.ToolDefinition{
				Name:        name,
				Description: strings.Repeat("d", descriptionBytes),
				InputSchema: []byte(`{"type":"object"}`),
			},
			Load: func(context.Context) (tool.Tool, error) {
				return tool.TextTool(message.ToolDefinition{
					Name:        name,
					Description: strings.Repeat("d", descriptionBytes),
					InputSchema: []byte(`{"type":"object"}`),
				}, func(context.Context, string) (string, error) { return "ok", nil }), nil
			},
		})
	}
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{lazyTools: lazy}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	return assembly
}

func runSearch(t *testing.T, assembly *engine.Assembly, limit int) string {
	t.Helper()
	view := assembly.NewCatalogView()
	ctx := engine.WithCatalogView(context.Background(), view)
	search, ok := assembly.Catalog().Get(engine.ToolName)
	if !ok {
		t.Fatal("tool_search is not registered")
	}
	out, err := search.Execute(ctx, fmt.Sprintf(`{"query":"tool search","limit":%d}`, limit))
	if err != nil {
		t.Fatalf("Execute tool_search: %v", err)
	}
	return jsonOf(t, out)
}

// TestToolSearchPayloadFitsTheRoundBudget: one search cannot hand the model more
// tool text than a round's definitions are allowed, however long the third-party
// descriptions are.
func TestToolSearchPayloadFitsTheRoundBudget(t *testing.T) {
	const tools, descriptionBytes = 24, 8 << 10
	assembly := deferredTools(t, tools, descriptionBytes)
	payload := runSearch(t, assembly, tools)

	if int64(len(payload)) > engine.DefaultBudget.MaxBytes {
		t.Fatalf("payload = %d bytes, want <= %d", len(payload), engine.DefaultBudget.MaxBytes)
	}
	// The cap trims text; it must not lose hits — a hit is a name the model can
	// still ask for, even with its description shortened.
	for i := range tools {
		if name := fmt.Sprintf("tool-%02d", i); !strings.Contains(payload, name) {
			t.Fatalf("hit %q missing from the answer (%d bytes)", name, len(payload))
		}
	}
	if strings.Contains(payload, strings.Repeat("d", descriptionBytes)) {
		t.Fatal("a description came back whole; the payload is not bounded")
	}
}

// TestToolSearchPayloadMeasuresEscaping: a description made of quotes and
// backslashes costs more inside JSON than it does as a Go string, and the budget
// is on the encoded bytes.
func TestToolSearchPayloadMeasuresEscaping(t *testing.T) {
	const tools, descriptionBytes = 24, 4 << 10
	assembly := deferredTools(t, tools, descriptionBytes)
	// Rewrite the placeholders to the worst case for a byte count: every byte is
	// escaped on the way out.
	lazy := make([]tool.LazyTool, 0, tools)
	for i := range tools {
		name := fmt.Sprintf("tool-%02d", i)
		description := strings.Repeat(`"\`, descriptionBytes/2)
		lazy = append(lazy, tool.LazyTool{
			Name: name,
			Placeholder: message.ToolDefinition{
				Name: name, Description: description, InputSchema: []byte(`{"type":"object"}`),
			},
			Load: func(context.Context) (tool.Tool, error) {
				return tool.TextTool(message.ToolDefinition{
					Name: name, Description: description, InputSchema: []byte(`{"type":"object"}`),
				}, func(context.Context, string) (string, error) { return "ok", nil }), nil
			},
		})
	}
	escaped, err := engine.NewAssembly(
		[]tool.Source{source{lazyTools: lazy}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	_ = assembly
	payload := runSearch(t, escaped, tools)
	if int64(len(payload)) > engine.DefaultBudget.MaxBytes {
		t.Fatalf("payload = %d bytes, want <= %d even with every byte escaped",
			len(payload), engine.DefaultBudget.MaxBytes)
	}
}

// TestToolSearchPayloadFollowsThePolicyBudget: the answer follows Budget.MaxBytes,
// so a host that narrows the round budget narrows the search answer with it.
func TestToolSearchPayloadFollowsThePolicyBudget(t *testing.T) {
	const tools, descriptionBytes = 8, 4 << 10
	lazy := make([]tool.LazyTool, 0, tools)
	for i := range tools {
		name := fmt.Sprintf("tool-%02d", i)
		lazy = append(lazy, tool.LazyTool{
			Name: name,
			Placeholder: message.ToolDefinition{
				Name: name, Description: strings.Repeat("d", descriptionBytes),
				InputSchema: []byte(`{"type":"object"}`),
			},
			Load: func(context.Context) (tool.Tool, error) {
				return tool.TextTool(message.ToolDefinition{
					Name: name, Description: strings.Repeat("d", descriptionBytes),
					InputSchema: []byte(`{"type":"object"}`),
				}, func(context.Context, string) (string, error) { return "ok", nil }), nil
			},
		})
	}
	narrow, err := engine.NewAssembly(
		[]tool.Source{source{lazyTools: lazy}},
		engine.WithDynamic(engine.Policy{
			Default: engine.ExposureDeferred,
			Budget:  engine.Budget{MaxBytes: 512},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	payload := runSearch(t, narrow, tools)
	if len(payload) > 512 {
		t.Fatalf("payload = %d bytes, want <= 512 (the policy's Budget.MaxBytes): %s", len(payload), payload)
	}
}

// TestToolSearchPayloadLeavesShortDescriptionsAlone: below the cap nothing is
// rewritten, so an ordinary catalog reads exactly as it did.
func TestToolSearchPayloadLeavesShortDescriptionsAlone(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{
			tool.TextTool(message.ToolDefinition{
				Name: "web.search", Description: "find things on the web",
				InputSchema: []byte(`{"type":"object"}`),
			}, func(context.Context, string) (string, error) { return "ok", nil }),
		}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	payload := runSearch(t, assembly, 8)
	if !strings.Contains(payload, `"hits":[`+"{") {
		t.Fatalf("no hits came back for a matching query: %s", payload)
	}
	if !strings.Contains(payload, "find things on the web") {
		t.Fatalf("a short description was rewritten: %s", payload)
	}
	if strings.Contains(payload, tool.DescriptionEllipsis) {
		t.Fatalf("a short description was marked as trimmed: %s", payload)
	}
}
