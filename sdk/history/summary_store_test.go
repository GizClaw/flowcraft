package history

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFileSummaryStore_SaveAndList(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	node := &SummaryNode{
		ConversationID: "conv1",
		Depth:          0,
		Content:        "test summary",
		EarliestSeq:    0,
		LatestSeq:      9,
		TokenCount:     10,
	}
	if err := store.Save(ctx, node); err != nil {
		t.Fatal(err)
	}
	if node.ID == "" {
		t.Fatal("expected ID to be set")
	}

	nodes, err := store.List(ctx, "conv1", SummaryListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1, got %d", len(nodes))
	}
	if nodes[0].Content != "test summary" {
		t.Fatalf("unexpected content: %q", nodes[0].Content)
	}
}

func TestFileSummaryStore_DepthFilter(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Depth: 0, Content: "d0", EarliestSeq: 0, LatestSeq: 5})
	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Depth: 1, Content: "d1", EarliestSeq: 0, LatestSeq: 10})

	d0 := intPtr(0)
	nodes, err := store.List(ctx, "c", SummaryListOptions{Depth: d0})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 d0 node, got %d", len(nodes))
	}
}

func TestFileSummaryStore_Delete(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	node := &SummaryNode{ConversationID: "c", Depth: 0, Content: "will delete", EarliestSeq: 0, LatestSeq: 5}
	_ = store.Save(ctx, node)

	if err := store.DeleteByConvID(ctx, "c", node.ID); err != nil {
		t.Fatal(err)
	}

	nodes, err := store.List(ctx, "c", SummaryListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(nodes))
	}
}

func TestFileSummaryStore_Rewrite(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ID: "a", ConversationID: "c", Content: "old"})
	_ = store.Save(ctx, &SummaryNode{ID: "b", ConversationID: "c", Content: "also old"})

	if err := store.Rewrite(ctx, "c", []*SummaryNode{
		{ID: "a", ConversationID: "c", Content: "rewritten"},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := store.ListAll(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 after rewrite, got %d", len(all))
	}
	if all[0].Content != "rewritten" {
		t.Fatalf("unexpected content: %q", all[0].Content)
	}
}

func TestFileSummaryStore_Search(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Content: "golang performance tuning", EarliestSeq: 0, LatestSeq: 5})
	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Content: "python data analysis setup", EarliestSeq: 6, LatestSeq: 10})

	results, err := store.Search(ctx, "c", "golang performance", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'golang performance'")
	}
	if results[0].Content != "golang performance tuning" {
		t.Fatalf("expected golang result first, got: %q", results[0].Content)
	}
}

func TestFileSummaryStore_CacheHit(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Content: "node1", EarliestSeq: 0, LatestSeq: 5})

	// First List populates cache.
	nodes1, err := store.List(ctx, "c", SummaryListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes1) != 1 {
		t.Fatalf("expected 1, got %d", len(nodes1))
	}

	if cached, ok := cacheSnapshot(store, "c"); !ok || len(cached) == 0 {
		t.Fatal("expected cache to be populated after List")
	}

	// Second List should use cache (same result without disk read).
	nodes2, err := store.List(ctx, "c", SummaryListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes2) != 1 {
		t.Fatalf("expected 1 from cache, got %d", len(nodes2))
	}
}

func TestFileSummaryStore_CacheUpdatedOnSave(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Content: "first", EarliestSeq: 0, LatestSeq: 5})
	_ = store.Save(ctx, &SummaryNode{ConversationID: "c", Content: "second", EarliestSeq: 6, LatestSeq: 10})

	cached, _ := cacheSnapshot(store, "c")
	if len(cached) != 2 {
		t.Fatalf("expected 2 in cache, got %d", len(cached))
	}
}

func TestFileSummaryStore_CacheUpdatedOnRewrite(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{ID: "a", ConversationID: "c", Content: "old1"})
	_ = store.Save(ctx, &SummaryNode{ID: "b", ConversationID: "c", Content: "old2"})

	_ = store.Rewrite(ctx, "c", []*SummaryNode{
		{ID: "a", ConversationID: "c", Content: "new1"},
	})

	cached, _ := cacheSnapshot(store, "c")
	if len(cached) != 1 {
		t.Fatalf("expected 1 in cache after rewrite, got %d", len(cached))
	}
	if cached[0].Content != "new1" {
		t.Fatalf("cache content should be updated, got %q", cached[0].Content)
	}
}

func TestFileSummaryStore_CacheUpdatedOnDelete(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	node := &SummaryNode{ConversationID: "c", Content: "to delete", EarliestSeq: 0, LatestSeq: 5}
	_ = store.Save(ctx, node)

	_ = store.DeleteByConvID(ctx, "c", node.ID)

	cached, _ := cacheSnapshot(store, "c")
	if len(cached) != 2 {
		t.Fatalf("expected 2 (original + delete marker), got %d", len(cached))
	}

	// But List should filter out deleted.
	nodes, _ := store.List(ctx, "c", SummaryListOptions{})
	if len(nodes) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(nodes))
	}
}

// cacheSnapshot reads the LRU-backed cache for the given conversation
// without going through the public API. It returns (cachedNodes, true) if
// the entry is present and loaded, (nil, false) otherwise.
func cacheSnapshot(s *FileSummaryStore, convID string) ([]*SummaryNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[convID]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*summaryCacheEntry)
	if !entry.loaded {
		return nil, false
	}
	cp := make([]*SummaryNode, len(entry.nodes))
	copy(cp, entry.nodes)
	return cp, true
}
