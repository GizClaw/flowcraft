package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// benchCatalog is a definitions-only catalog: it answers the read side the
// search path actually uses and nothing else. A real deployment puts a
// *tool.Registry here (which also carries DefinitionsEpoch); the fixture keeps
// the benchmark measuring the ranking work rather than the registry.
type benchCatalog struct {
	defs []message.ToolDefinition
}

func (c benchCatalog) Get(string) (tool.Tool, bool) { return nil, false }

func (c benchCatalog) Definitions() []message.ToolDefinition {
	out := make([]message.ToolDefinition, len(c.defs))
	copy(out, c.defs)
	return out
}

// benchCatalogSize models a deployment that exposed an MCP server's tool list:
// long names, real descriptions, a JSON Schema each.
func benchCatalogOf(size int) benchCatalog {
	schema := `{"type":"object","properties":{` +
		`"query":{"type":"string","description":"free-text query"},` +
		`"status":{"type":"string","enum":["open","closed"]},` +
		`"owner":{"type":"string"},` +
		`"cursor":{"type":"string","description":"pagination cursor"}}}`
	defs := make([]message.ToolDefinition, 0, size)
	for i := 0; i < size; i++ {
		defs = append(defs, message.ToolDefinition{
			Name: fmt.Sprintf("mcp_connector_%d_search", i),
			Description: fmt.Sprintf(
				"Search records in connector %d by free-text query, returning ids, titles "+
					"and relevance scores. Supports filters for status, owner and an updated "+
					"range; results are paginated with a cursor.", i),
			InputSchema: []byte(schema),
		})
	}
	return benchCatalog{defs: defs}
}

func benchView(size int) *dynamicView {
	policy := DefaultPolicy()
	policy.Default = ExposureDeferred
	return &dynamicView{
		catalog: benchCatalogOf(size),
		policy:  policy,
		st:      *newViewState(),
	}
}

// BenchmarkToolSearch is the cost of one tool_search call over a catalog of
// this size. It is the whole path — definitions, doc building, index, ranking —
// so an index cache shows up here as a step change.
func BenchmarkToolSearch(b *testing.B) {
	for _, size := range []int{50, 200, 500} {
		b.Run(fmt.Sprintf("tools=%d", size), func(b *testing.B) {
			view := benchView(size)
			ctx := context.Background()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := view.search(ctx, "search connector status filter", 8); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkToolSearchRepeat is the same call twice in a row: the second one is
// what a cached index answers, so the gap between it and BenchmarkToolSearch is
// the rebuild cost.
func BenchmarkToolSearchRepeat(b *testing.B) {
	view := benchView(200)
	ctx := context.Background()
	// Warm whatever cache the view has before timing.
	if _, err := view.search(ctx, "warm", 8); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := view.search(ctx, "search connector status filter", 8); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDefinitionsScaling is the per-round cost of handing the visible set
// to the model, at a catalog size where the derivation is visible. Everything is
// ExposureAlways so the round has a full visible set to derive.
func BenchmarkDefinitionsScaling(b *testing.B) {
	for _, size := range []int{50, 200, 500} {
		b.Run(fmt.Sprintf("tools=%d", size), func(b *testing.B) {
			view := benchView(size)
			view.policy.Default = ExposureAlways
			view.policy.Budget = Budget{MaxDefinitions: 1024, MaxBytes: 4 << 20}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if len(view.Definitions()) == 0 {
					b.Fatal("no definitions")
				}
			}
		})
	}
}
