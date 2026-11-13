package engine

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// searchFixtureTool is the minimal tool a registry search needs: a definition
// with a description, and an Execute nothing in these tests reaches.
type searchFixtureTool struct{ def message.ToolDefinition }

func (t searchFixtureTool) Definition() message.ToolDefinition { return t.def }

func (t searchFixtureTool) Execute(context.Context, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

func searchFixtureSource(names ...string) tool.Source {
	tools := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, searchFixtureTool{def: message.ToolDefinition{
			Name:        name,
			Description: "Search records by free-text query and return ranked matches.",
			InputSchema: []byte(`{"type":"object"}`),
		}})
	}
	return staticSource{tools: tools}
}

type staticSource struct{ tools []tool.Tool }

func (s staticSource) Tools() []tool.Tool         { return s.tools }
func (s staticSource) LazyTools() []tool.LazyTool { return nil }

// TestToolSearchReusesIndex: the second search over an unchanged catalog must
// answer from the index the first one built. A rebuild is the expensive half of
// tool_search, so re-tokenizing every description per call is the regression
// this guards.
func TestToolSearchReusesIndex(t *testing.T) {
	assembly, err := NewAssembly(
		[]tool.Source{searchFixtureSource("read_file", "write_file", "grep")},
		WithDynamic(Policy{Default: ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	defer assembly.Close()
	view := assembly.NewCatalogView().(*dynamicView)
	ctx := context.Background()

	if _, err := view.search(ctx, "read a file", 0); err != nil {
		t.Fatalf("first search: %v", err)
	}
	built := assembly.searchCache.built
	if built == nil {
		t.Fatal("first search did not cache an index")
	}
	if _, err := view.search(ctx, "read a file", 0); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if assembly.searchCache.built != built {
		t.Fatal("second search rebuilt the index")
	}
}

// TestToolSearchSeesAddedTool: the cache is only safe because the registry's
// definition epoch moves on every mutation. A tool registered after the first
// search — an MCP server refreshing its list — must be findable.
func TestToolSearchSeesAddedTool(t *testing.T) {
	assembly, err := NewAssembly(
		[]tool.Source{searchFixtureSource("read_file")},
		WithDynamic(Policy{Default: ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	defer assembly.Close()
	view := assembly.NewCatalogView().(*dynamicView)
	ctx := context.Background()

	if _, err := view.search(ctx, "kanban board", 0); err != nil {
		t.Fatalf("search before add: %v", err)
	}
	if err := assembly.registry.Add(searchFixtureTool{def: message.ToolDefinition{
		Name:        "kanban_board",
		Description: "Read the kanban board and return its columns.",
		InputSchema: []byte(`{"type":"object"}`),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hits, err := view.search(ctx, "kanban board", 0)
	if err != nil {
		t.Fatalf("search after add: %v", err)
	}
	for _, hit := range hits {
		if hit.Name == "kanban_board" {
			return
		}
	}
	t.Fatalf("search after Add = %v, want the new tool", hits)
}

// TestToolSearchRebuildsWithoutEpoch: a catalog that cannot report a definition
// epoch keeps the pre-cache behaviour — rebuild per call — rather than serving a
// result nobody can prove is current.
func TestToolSearchRebuildsWithoutEpoch(t *testing.T) {
	cache := &searchIndexCache{}
	policy := DefaultPolicy()
	policy.Default = ExposureDeferred
	view := &dynamicView{
		catalog:     benchCatalogOf(3),
		policy:      policy,
		st:          *newViewState(),
		searchCache: cache,
	}
	ctx := context.Background()
	if _, err := view.search(ctx, "connector", 0); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if cache.built != nil {
		t.Fatal("a catalog without DefinitionsEpoch must not populate the cache")
	}
}
