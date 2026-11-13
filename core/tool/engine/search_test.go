package engine

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The kernel is shared (core/utils/bm25); the catalog's own contract is
// what it hands the kernel and how it keys the results.

func TestCatalogTermsDropSingleRuneASCII(t *testing.T) {
	got := catalogTerms("Read a file 1 世界")
	want := []string{"read", "file", "世", "世界", "界"}
	if !slices.Equal(got, want) {
		t.Fatalf("catalogTerms = %v, want %v", got, want)
	}
}

// rankDocs is the seam these ranking tests use: build an index over docs and run
// one query. It is the path tool_search takes, minus the assembly cache.
func rankDocs(docs []searchDoc, query string, limit int) ([]SearchHit, error) {
	index, err := buildSearchIndex(docs)
	if err != nil {
		return nil, err
	}
	return index.search(query, limit), nil
}

func catalogDocs() []searchDoc {
	return []searchDoc{
		{name: "read_file", description: "Read a file from the workspace"},
		{name: "write_file", description: "Write a file into the workspace"},
		{name: "webfetch", description: "Fetch a URL over HTTP"},
		{name: "清理会话", description: "清理当前会话的历史记录"},
	}
}

func hitNames(hits []SearchHit) []string {
	names := make([]string, 0, len(hits))
	for _, hit := range hits {
		names = append(names, hit.Name)
	}
	return names
}

// TestSearchIgnoresFillerWordsInQueries pins what the catalog's two-rune
// minimum buys. Without it a bare "a" is indexed and prefix-expands to
// every term starting with "a", so "read a file" dragged webfetch into the
// hit set — and every hit is loaded and takes a discovery-pool slot, which
// is a tool the model was using pushed out for a round.
func TestSearchIgnoresFillerWordsInQueries(t *testing.T) {
	hits, err := rankDocs(catalogDocs(), "read a file", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if names := hitNames(hits); !slices.Contains(names, "read_file") || slices.Contains(names, "webfetch") {
		t.Fatalf("rankDocs(read a file) = %v, want read_file and no webfetch", names)
	}
	for _, query := range []string{"a", "1", "a 1"} {
		hits, err := rankDocs(catalogDocs(), query, 0)
		if err != nil {
			t.Fatalf("rankDocs(%q): %v", query, err)
		}
		if len(hits) != 0 {
			t.Fatalf("rankDocs(%q) = %v, want no hits", query, hitNames(hits))
		}
	}
}

func TestSearchPrefixHitsSurfaceNames(t *testing.T) {
	hits, err := rankDocs(catalogDocs(), "webf", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "webfetch" {
		t.Fatalf("rankDocs(webf) = %v, want [webfetch]", hitNames(hits))
	}
}

// TestSearchMatchesCJKQueries covers the path the catalog did not have
// before the kernel: a Chinese query matches the run of characters it
// mentions and any fragment of it, without a segmentation dictionary.
func TestSearchMatchesCJKQueries(t *testing.T) {
	for _, query := range []string{"清理会话", "会话", "清理", "历史记录"} {
		hits, err := rankDocs(catalogDocs(), query, 0)
		if err != nil {
			t.Fatalf("rankDocs(%q): %v", query, err)
		}
		if len(hits) == 0 || hits[0].Name != "清理会话" {
			t.Fatalf("rankDocs(%q) = %v, want 清理会话 first", query, hitNames(hits))
		}
	}
}

func TestSearchNameOutranksText(t *testing.T) {
	hits, err := rankDocs([]searchDoc{
		{name: "alpha", description: "unrelated wording"},
		{name: "beta", description: "alpha in the description"},
	}, "alpha", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if names := hitNames(hits); len(names) != 2 || names[0] != "alpha" {
		t.Fatalf("rankDocs(alpha) = %v, want the name hit first", names)
	}
}

// TestSearchKeepsCallerOrder pins that ranking never reorders the slice a
// caller passed in: the kernel resolves ties by input order, and callers
// reuse cached definition slices.
func TestSearchKeepsCallerOrder(t *testing.T) {
	docs := catalogDocs()
	before := slices.Clone(docs)
	if _, err := rankDocs(docs, "file", 0); err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if !slices.Equal(docs, before) {
		t.Fatalf("bm25Search reordered its input: %v", docs)
	}
}

func TestSearchDefaultLimit(t *testing.T) {
	docs := make([]searchDoc, 0, 20)
	for i := range 20 {
		docs = append(docs, searchDoc{
			name:        fmt.Sprintf("tool_%02d", i),
			description: "shared capability",
		})
	}
	hits, err := rankDocs(docs, "capability", 0)
	if err != nil {
		t.Fatalf("bm25Search: %v", err)
	}
	if len(hits) != defaultSearchLimit {
		t.Fatalf("bm25Search with no limit = %d hits, want %d", len(hits), defaultSearchLimit)
	}
	if hits[0].Name != "tool_00" {
		t.Fatalf("bm25Search tie-break = %v, want the first name", hitNames(hits))
	}
}

func TestSearchEmptyQueryAndCatalog(t *testing.T) {
	for _, query := range []string{"", "   ", "!!!"} {
		hits, err := rankDocs(catalogDocs(), query, 0)
		if err != nil || len(hits) != 0 {
			t.Fatalf("rankDocs(%q) = %v, %v, want no hits", query, hits, err)
		}
	}
	if hits, err := rankDocs(nil, "read", 0); err != nil || len(hits) != 0 {
		t.Fatalf("bm25Search on an empty catalog = %v, %v, want no hits", hits, err)
	}
}

// searchFixtureTool is the minimal tool a registry search needs: a definition
// with a description, and an Execute nothing in these tests reaches.
type searchFixtureTool struct{ def message.ToolDefinition }

func (t searchFixtureTool) Definition() message.ToolDefinition { return t.def }

func (t searchFixtureTool) Execute(context.Context, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

type staticSource struct{ tools []tool.Tool }

func (s staticSource) Tools() []tool.Tool         { return s.tools }
func (s staticSource) LazyTools() []tool.LazyTool { return nil }

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
	defer func() { _ = assembly.Close() }()
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
	defer func() { _ = assembly.Close() }()
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
// result nobody can prove is current. The assertion is behavioural: a definition
// added between two searches must be visible to the second one.
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
	if _, err := view.search(ctx, "kanban board", 0); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if cache.built != nil {
		t.Fatal("a catalog without DefinitionsEpoch must not populate the cache")
	}

	// The fixture catalog is a value; replace it the way a refresh would.
	grown := append(benchDefinitions(3), message.ToolDefinition{
		Name:        "kanban_board",
		Description: "Read the kanban board and return its columns.",
		InputSchema: []byte(benchSchema),
	})
	view.catalog = benchCatalog{defs: grown}
	hits, err := view.search(ctx, "kanban board", 0)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	for _, hit := range hits {
		if hit.Name == "kanban_board" {
			return
		}
	}
	t.Fatalf("search after the catalog grew = %v, want the new tool", hits)
}

// TestToolSearchAllocationsStable guards the cache by comparing the two paths
// over the same fixture: a cached search is a ranking pass over the built index
// (tens of allocations), while a rebuild tokenizes every name and description and
// builds the postings. The ratio is what makes the guard meaningful — a bare
// ceiling cannot tell a cache hit from a rebuild of a small catalog.
func TestToolSearchAllocationsStable(t *testing.T) {
	assembly, err := NewAssembly(
		[]tool.Source{searchFixtureSource(
			"read_file", "write_file", "grep", "glob", "list_dir", "edit_file",
			"kanban_board", "calendar_list", "mail_search", "issue_search",
		)},
		WithDynamic(Policy{Default: ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	defer func() { _ = assembly.Close() }()
	view := assembly.NewCatalogView()
	ctx := context.Background()
	if _, err := view.Search(ctx, "search records", 0); err != nil {
		t.Fatalf("warm search: %v", err)
	}
	warm := testing.AllocsPerRun(100, func() {
		if _, err := view.Search(ctx, "search records", 0); err != nil {
			t.Fatalf("search: %v", err)
		}
	})

	// The same registry without a cache: one rebuild per call.
	uncached := &dynamicView{
		catalog: assembly.registry,
		policy:  view.(*dynamicView).policy,
		st:      *newViewState(),
	}
	cold := testing.AllocsPerRun(10, func() {
		if _, err := uncached.search(ctx, "search records", 0); err != nil {
			t.Fatalf("search: %v", err)
		}
	})

	if warm > 200 {
		t.Fatalf("cached tool_search allocates %.0f times per call, want <= 200", warm)
	}
	if warm*3 > cold {
		t.Fatalf("cached tool_search allocates %.0f times, a rebuild %.0f: the cache is not being used", warm, cold)
	}
}
