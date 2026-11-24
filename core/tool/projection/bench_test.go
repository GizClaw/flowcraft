package projection_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/projection"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// A projection runs on every turn: Definitions is what a turn costs to start, and
// tool_search runs whenever the model goes looking. The benchmarks here are sized like a
// real catalog — tens of tools, not three — because the visibility computation is linear in
// the catalog and the search index is built over all of it.

const benchTools = 60

func benchCatalog(b *testing.B) *tool.Registry {
	b.Helper()
	names := make([]string, 0, benchTools)
	for i := range benchTools {
		names = append(names, fmt.Sprintf("tool_%02d", i))
	}
	registry, err := tool.NewRegistry(nil)
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	for i, name := range names {
		def := messageDef(name, fmt.Sprintf("performs operation %d on the workspace", i))
		if err := registry.Add(tool.TextTool(def, func(context.Context, string) (string, error) {
			return name, nil
		})); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
	return registry
}

func messageDef(name, description string) message.ToolDefinition {
	return message.ToolDefinition{
		Name:        name,
		Description: description,
		InputSchema: []byte(`{"type":"object"}`),
	}
}

func benchPolicy() projection.Policy {
	return projection.Policy{
		Default: projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{
			projection.ToolName: projection.ExposureAlways,
		},
		Budget:    projection.Budget{MaxDefinitions: 16, MaxBytes: 16 << 10},
		Discovery: projection.DiscoveryPolicy{MaxTools: 16, MaxBytes: 16 << 10, IdleRounds: 10},
	}
}

// BenchmarkDefinitions is the per-turn floor: the visible set of a large catalog with a
// handful of discovered tools, computed from the cached snapshot.
func BenchmarkDefinitions(b *testing.B) {
	p, err := projection.New(benchCatalog(b), benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	view.Discover("tool_00", "tool_01", "tool_02", "tool_03")

	b.ReportAllocs()
	for b.Loop() {
		if len(view.Definitions()) == 0 {
			b.Fatal("no definitions")
		}
	}
}

// BenchmarkDefinitionsColdCatalog measures the rebuild: a catalog whose definition epoch
// moved on every call (an MCP server re-listing), so the snapshot cannot be reused.
func BenchmarkDefinitionsColdCatalog(b *testing.B) {
	registry := benchCatalog(b)
	p, err := projection.New(registry, benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	b.ReportAllocs()
	for b.Loop() {
		// Add and remove one tool: the epoch moves, the snapshot must be rebuilt.
		if err := registry.Add(tool.TextTool(messageDef("churn", "churn"), func(context.Context, string) (string, error) {
			return "", nil
		})); err != nil {
			b.Fatalf("Add: %v", err)
		}
		_ = view.Definitions()
		registry.Remove("churn")
	}
}

// BenchmarkVisible is the eligibility check a host or a middleware performs for one name.
func BenchmarkVisible(b *testing.B) {
	p, err := projection.New(benchCatalog(b), benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	view.Discover("tool_07")
	b.ReportAllocs()
	for b.Loop() {
		if !view.Visible("tool_07") {
			b.Fatal("the discovered tool is not visible")
		}
	}
}

// BenchmarkDiscover is the cost of one tool_search answer entering the pool: ranking,
// eviction and the exposure report.
func BenchmarkDiscover(b *testing.B) {
	p, err := projection.New(benchCatalog(b), benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	b.ReportAllocs()
	for b.Loop() {
		view.Discover("tool_00", "tool_01", "tool_02", "tool_03", "tool_04")
		view.AdvanceRound()
	}
}

// BenchmarkSearch measures a tool_search ranking over the whole catalog. The index is
// built once and reused while the catalog stands still, so this is the query cost alone.
func BenchmarkSearch(b *testing.B) {
	p, err := projection.New(benchCatalog(b), benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	if _, err := view.Search(context.Background(), "workspace operation", 8); err != nil {
		b.Fatalf("warm-up Search: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		hits, err := view.Search(context.Background(), "workspace operation", 8)
		if err != nil || len(hits) == 0 {
			b.Fatalf("Search = %d hits, %v", len(hits), err)
		}
	}
}

// BenchmarkToolSearchExecute is the whole discovery call: rank, load, discover, fit the
// answer to the round's byte budget, encode.
func BenchmarkToolSearchExecute(b *testing.B) {
	p, err := projection.New(benchCatalog(b), benchPolicy())
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	view := p.NewView()
	binding := tool.Bind(tooltest.Session("bench"), policy.Grant{}).WithView(view)
	arguments := `{"query":"workspace operation","limit":8}`
	b.ReportAllocs()
	for b.Loop() {
		if _, err := (projection.SearchTool{}).Execute(context.Background(), binding, arguments); err != nil {
			b.Fatalf("Execute: %v", err)
		}
	}
}
