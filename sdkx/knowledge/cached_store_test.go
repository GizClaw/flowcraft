package knowledge

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestCachedStore_GetDocument_CacheHit(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner, WithTTL(time.Minute), WithMaxItems(10))
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")

	// First call: miss
	doc1, err := cached.GetDocument(ctx, "ds1", "doc.md")
	if err != nil {
		t.Fatal(err)
	}

	// Second call: hit (from cache)
	doc2, err := cached.GetDocument(ctx, "ds1", "doc.md")
	if err != nil {
		t.Fatal(err)
	}

	if doc1.Content != doc2.Content {
		t.Fatal("cached content mismatch")
	}
}

func TestCachedStore_AddEvictsCache(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "original")

	// Cache the document
	_, _ = cached.GetDocument(ctx, "ds1", "doc.md")

	// Add new version through cached store
	_ = cached.AddDocument(ctx, "ds1", "doc.md", "updated")

	// Should return fresh content (cache evicted)
	doc, _ := cached.GetDocument(ctx, "ds1", "doc.md")
	if doc.Content != "updated" {
		t.Fatalf("expected updated content, got %q", doc.Content)
	}
}

func TestCachedStore_DeleteEvictsCache(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")
	_, _ = cached.GetDocument(ctx, "ds1", "doc.md")

	_ = cached.DeleteDocument(ctx, "ds1", "doc.md")

	_, err := cached.GetDocument(ctx, "ds1", "doc.md")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestCachedStore_Search(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "Go programming language")

	results1, _ := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})
	results2, _ := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})

	if len(results1) != len(results2) {
		t.Fatalf("cached search results mismatch: %d vs %d", len(results1), len(results2))
	}
}

func TestCachedStore_TTLExpiry(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner, WithTTL(1*time.Millisecond))
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")
	_, _ = cached.GetDocument(ctx, "ds1", "doc.md")

	time.Sleep(5 * time.Millisecond)

	// Should miss cache (expired)
	doc, _ := cached.GetDocument(ctx, "ds1", "doc.md")
	if doc == nil {
		t.Fatal("expected document even after cache expiry")
	}
}

func TestCachedStore_MaxItems(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner, WithMaxItems(2))
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "a.md", "doc a")
	_ = inner.AddDocument(ctx, "ds1", "b.md", "doc b")
	_ = inner.AddDocument(ctx, "ds1", "c.md", "doc c")

	_, _ = cached.GetDocument(ctx, "ds1", "a.md")
	_, _ = cached.GetDocument(ctx, "ds1", "b.md")
	_, _ = cached.GetDocument(ctx, "ds1", "c.md") // should evict oldest

	// All should still be readable (miss will re-fetch)
	doc, _ := cached.GetDocument(ctx, "ds1", "a.md")
	if doc == nil {
		t.Fatal("expected document a")
	}
}

func TestCachedStore_SearchModeCacheIsolation(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "Go programming language features")

	// Query with BM25 mode
	resultsBM25, err := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeBM25})
	if err != nil {
		t.Fatal(err)
	}
	if len(resultsBM25) == 0 {
		t.Fatal("expected BM25 results")
	}

	// Query with different mode — should NOT use BM25 cache
	resultsHybrid, err := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}

	// Re-query BM25 — should hit cache and match original
	resultsBM25Again, _ := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeBM25})
	if len(resultsBM25Again) != len(resultsBM25) {
		t.Fatalf("BM25 cache mismatch: %d vs %d", len(resultsBM25Again), len(resultsBM25))
	}

	// Re-query hybrid — should hit its own (separate) cache
	resultsHybridAgain, _ := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeHybrid})
	if len(resultsHybridAgain) != len(resultsHybrid) {
		t.Fatalf("hybrid cache mismatch: %d vs %d", len(resultsHybridAgain), len(resultsHybrid))
	}
}

func TestCachedStore_GetDocument_WrongCacheType_NosPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner, WithTTL(time.Minute), WithMaxItems(10))
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")

	// Prime the cache
	_, _ = cached.GetDocument(ctx, "ds1", "doc.md")

	// Corrupt the cache entry with a wrong type
	key := "doc:ds1/doc.md"
	cached.set(key, 12345) // int instead of Document

	// Should not panic, should fall back to inner store
	doc, err := cached.GetDocument(ctx, "ds1", "doc.md")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if doc.Content != "content" {
		t.Fatalf("expected content from inner store, got %q", doc.Content)
	}
}

func TestCachedStore_Search_WrongCacheType_NoPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "Go programming language")

	// Prime the cache
	_, _ = cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})

	// Corrupt with wrong type
	key := "search:ds1|Go programming|5||0.000000|"
	cached.set(key, "not-a-slice")

	// Should not panic
	results, err := cached.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	_ = results
}

func TestCachedStore_Abstract_WrongCacheType_NoPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")
	inner.SetDocAbstract("ds1", "doc.md", "abstract")

	// Prime
	_, _ = cached.Abstract(ctx, "ds1", "doc.md")

	// Corrupt
	key := "abs:ds1/doc.md"
	cached.set(key, 999)

	abs, err := cached.Abstract(ctx, "ds1", "doc.md")
	if err != nil {
		t.Fatalf("expected fallback, got error: %v", err)
	}
	if abs != "abstract" {
		t.Fatalf("expected 'abstract', got %q", abs)
	}
}

func TestCachedStore_AbstractOverview(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds1", "doc.md", "content")
	inner.SetDocAbstract("ds1", "doc.md", "abstract")
	inner.SetDocOverview("ds1", "doc.md", "overview")

	abs, _ := cached.Abstract(ctx, "ds1", "doc.md")
	if abs != "abstract" {
		t.Fatalf("expected abstract, got %q", abs)
	}

	ov, _ := cached.Overview(ctx, "ds1", "doc.md")
	if ov != "overview" {
		t.Fatalf("expected overview, got %q", ov)
	}

	// Second call should be cached
	abs2, _ := cached.Abstract(ctx, "ds1", "doc.md")
	if abs2 != "abstract" {
		t.Fatalf("cache hit failed")
	}
}
