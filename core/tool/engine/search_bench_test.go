package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// benchSchema is the shape a connector tool ships: a few described properties.
const benchSchema = `{"type":"object","properties":{` +
	`"query":{"type":"string","description":"free-text query"},` +
	`"status":{"type":"string","enum":["open","closed"]},` +
	`"owner":{"type":"string"},` +
	`"cursor":{"type":"string","description":"pagination cursor"}}}`

// benchTool is a definition-only tool: enough for the search path, and cheap
// enough that a 500-tool registry stays a fixture rather than a workload.
type benchTool struct{ def message.ToolDefinition }

func (t benchTool) Definition() message.ToolDefinition { return t.def }

func (t benchTool) Execute(context.Context, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

type benchSource struct{ tools []tool.Tool }

func (s benchSource) Tools() []tool.Tool         { return s.tools }
func (s benchSource) LazyTools() []tool.LazyTool { return nil }

// benchDefinitions models a deployment that exposed an MCP server's tool list:
// long names, real descriptions, a JSON Schema each.
func benchDefinitions(size int) []message.ToolDefinition {
	defs := make([]message.ToolDefinition, 0, size)
	for i := 0; i < size; i++ {
		defs = append(defs, message.ToolDefinition{
			Name: fmt.Sprintf("mcp_connector_%d_search", i),
			Description: fmt.Sprintf(
				"Search records in connector %d by free-text query, returning ids, titles "+
					"and relevance scores. Supports filters for status, owner and an updated "+
					"range; results are paginated with a cursor.", i),
			InputSchema: []byte(benchSchema),
		})
	}
	return defs
}

// benchRegistry is a real registry, because the index cache is keyed by the
// registry's definition epoch: a stub catalog would not be cacheable at all.
func benchRegistry(tb testing.TB, size int) *tool.Registry {
	tb.Helper()
	defs := benchDefinitions(size)
	tools := make([]tool.Tool, 0, len(defs))
	for _, def := range defs {
		tools = append(tools, benchTool{def: def})
	}
	registry, err := tool.NewRegistry([]tool.Source{benchSource{tools: tools}})
	if err != nil {
		tb.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

// benchView builds a view over a real registry. A nil cache is the
// rebuild-per-call baseline; a shared one is what an assembly hands out.
func benchView(tb testing.TB, size int, cache *searchIndexCache) *dynamicView {
	tb.Helper()
	registry := benchRegistry(tb, size)
	tb.Cleanup(func() { _ = registry.Close() })
	policy := DefaultPolicy()
	policy.Default = ExposureDeferred
	return &dynamicView{
		catalog:     registry,
		policy:      policy,
		st:          *newViewState(),
		searchCache: cache,
	}
}

// BenchmarkToolSearch is the cost of one tool_search call over a catalog of this
// size. The two variants differ only in whether the index is kept: "rebuild" is
// what every call paid before the cache existed, "cached" is what a second search
// pays now.
func BenchmarkToolSearch(b *testing.B) {
	for _, size := range []int{50, 200, 500} {
		for _, variant := range []struct {
			name  string
			cache func() *searchIndexCache
		}{
			{"rebuild", func() *searchIndexCache { return nil }},
			{"cached", func() *searchIndexCache { return &searchIndexCache{} }},
		} {
			b.Run(fmt.Sprintf("%s/tools=%d", variant.name, size), func(b *testing.B) {
				view := benchView(b, size, variant.cache())
				ctx := context.Background()
				// Warm the cache before timing so the first search is not
				// amortized into the number.
				if _, err := view.search(ctx, "warm", 0); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := view.search(ctx, "search connector status filter", 8); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkDefinitionsScaling is the per-round cost of handing the visible set to
// the model, at catalog sizes where the derivation is measurable. Everything is
// ExposureAlways so the round has a full set to derive.
func BenchmarkDefinitionsScaling(b *testing.B) {
	for _, size := range []int{50, 200, 500} {
		b.Run(fmt.Sprintf("tools=%d", size), func(b *testing.B) {
			view := benchView(b, size, &searchIndexCache{})
			view.policy.Default = ExposureAlways
			view.policy.Budget = Budget{MaxDefinitions: 1024, MaxBytes: 4 << 20}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(view.Definitions()) == 0 {
					b.Fatal("no definitions")
				}
			}
		})
	}
}

// BenchmarkSearchIndexBuild isolates the half the cache removes: tokenizing
// every name and description and building the postings.
func BenchmarkSearchIndexBuild(b *testing.B) {
	docs := searchDocs(benchDefinitions(200))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildSearchIndex(docs); err != nil {
			b.Fatal(err)
		}
	}
}

// benchCatalog is a definitions-only catalog with no definition epoch: the shape
// a fixture or a thin wrapper has, where the index cannot be reused safely.
type benchCatalog struct{ defs []message.ToolDefinition }

func (c benchCatalog) Get(string) (tool.Tool, bool) { return nil, false }

func (c benchCatalog) Definitions() []message.ToolDefinition {
	out := make([]message.ToolDefinition, len(c.defs))
	copy(out, c.defs)
	return out
}

func benchCatalogOf(size int) benchCatalog {
	return benchCatalog{defs: benchDefinitions(size)}
}
