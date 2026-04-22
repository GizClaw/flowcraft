package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// errStore is a Store that fails every read/write with the configured error.
type errStore struct{ err error }

func (e errStore) AddDocument(context.Context, string, string, string) error {
	return e.err
}
func (e errStore) AddDocuments(context.Context, string, []DocInput) error { return e.err }
func (e errStore) GetDocument(context.Context, string, string) (*Document, error) {
	return nil, e.err
}
func (e errStore) DeleteDocument(context.Context, string, string) error { return e.err }
func (e errStore) ListDocuments(context.Context, string) ([]Document, error) {
	return nil, e.err
}
func (e errStore) Search(context.Context, string, string, SearchOptions) ([]SearchResult, error) {
	return nil, e.err
}
func (e errStore) Abstract(context.Context, string, string) (string, error) {
	return "", e.err
}
func (e errStore) Overview(context.Context, string, string) (string, error) {
	return "", e.err
}
func (e errStore) DatasetAbstract(context.Context, string) (string, error) {
	return "", e.err
}
func (e errStore) DatasetOverview(context.Context, string) (string, error) {
	return "", e.err
}

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

func TestCachedStore_AddDocumentsEvictsCache(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "a.md", "old a")
	_, _ = cached.GetDocument(ctx, "ds", "a.md")

	if err := cached.AddDocuments(ctx, "ds", []DocInput{
		{Name: "a.md", Content: "new a"},
		{Name: "b.md", Content: "new b"},
	}); err != nil {
		t.Fatal(err)
	}

	doc, _ := cached.GetDocument(ctx, "ds", "a.md")
	if doc.Content != "new a" {
		t.Fatalf("expected refreshed content, got %q", doc.Content)
	}
}

func TestCachedStore_AddDocumentsPropagatesError(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)

	err := cached.AddDocuments(context.Background(), "", []DocInput{{Name: "x.md", Content: "y"}})
	if err == nil {
		t.Fatal("expected error for empty datasetID")
	}
}

func TestCachedStore_ListDocumentsForwards(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "a.md", "a")
	_ = inner.AddDocument(ctx, "ds", "b.md", "b")

	docs, err := cached.ListDocuments(ctx, "ds")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
}

func TestCachedStore_DatasetAbstractCachesAndEvicts(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "doc.md", "body")
	inner.SetDatasetAbstract("ds", "v1")

	// Prime cache.
	if got, _ := cached.DatasetAbstract(ctx, "ds"); got != "v1" {
		t.Fatalf("got %q", got)
	}

	// Mutate inner without going through cached: cache should still serve v1.
	inner.SetDatasetAbstract("ds", "v2")
	if got, _ := cached.DatasetAbstract(ctx, "ds"); got != "v1" {
		t.Fatalf("expected cached v1, got %q", got)
	}

	// Explicit eviction surfaces the new value.
	cached.EvictDataset("ds")
	if got, _ := cached.DatasetAbstract(ctx, "ds"); got != "v2" {
		t.Fatalf("expected v2 after eviction, got %q", got)
	}
}

func TestCachedStore_DatasetOverviewCachesAndEvicts(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "doc.md", "body")
	inner.SetDatasetOverview("ds", "v1")

	if got, _ := cached.DatasetOverview(ctx, "ds"); got != "v1" {
		t.Fatalf("got %q", got)
	}

	inner.SetDatasetOverview("ds", "v2")
	if got, _ := cached.DatasetOverview(ctx, "ds"); got != "v1" {
		t.Fatalf("expected cached v1, got %q", got)
	}

	cached.EvictDataset("ds")
	if got, _ := cached.DatasetOverview(ctx, "ds"); got != "v2" {
		t.Fatalf("expected v2 after eviction, got %q", got)
	}
}

func TestCachedStore_DatasetAbstract_WrongCacheType_NoPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "doc.md", "body")
	inner.SetDatasetAbstract("ds", "real")
	_, _ = cached.DatasetAbstract(ctx, "ds")

	cached.set("dsabs:ds", 12345)

	got, err := cached.DatasetAbstract(ctx, "ds")
	if err != nil {
		t.Fatalf("expected fallback to inner, got error: %v", err)
	}
	if got != "real" {
		t.Fatalf("expected 'real' from inner store, got %q", got)
	}
}

func TestCachedStore_DatasetOverview_WrongCacheType_NoPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "doc.md", "body")
	inner.SetDatasetOverview("ds", "real")
	_, _ = cached.DatasetOverview(ctx, "ds")

	cached.set("dsov:ds", "ok-but-should-be-bypassed-only-on-wrong-type")
	// Force the wrong-type branch.
	cached.set("dsov:ds", []int{1, 2, 3})

	got, err := cached.DatasetOverview(ctx, "ds")
	if err != nil {
		t.Fatalf("expected fallback to inner, got error: %v", err)
	}
	if got != "real" {
		t.Fatalf("expected 'real' from inner store, got %q", got)
	}
}

func TestCachedStore_Overview_WrongCacheType_NoPanic(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	inner := NewFSStore(ws)
	cached := NewCachedStore(inner)
	ctx := context.Background()

	_ = inner.AddDocument(ctx, "ds", "doc.md", "body")
	inner.SetDocOverview("ds", "doc.md", "ov")
	_, _ = cached.Overview(ctx, "ds", "doc.md")

	cached.set("ov:ds/doc.md", 999)

	got, err := cached.Overview(ctx, "ds", "doc.md")
	if err != nil {
		t.Fatalf("expected fallback to inner, got error: %v", err)
	}
	if got != "ov" {
		t.Fatalf("expected 'ov' from inner store, got %q", got)
	}
}

func TestCachedStore_Search_PropagatesError(t *testing.T) {
	cached := NewCachedStore(errStore{err: errors.New("boom")})
	if _, err := cached.Search(context.Background(), "ds", "q", SearchOptions{TopK: 1}); err == nil {
		t.Fatal("expected error to bubble up")
	}
}

func TestCachedStore_Abstract_PropagatesError(t *testing.T) {
	cached := NewCachedStore(errStore{err: errors.New("boom")})
	if _, err := cached.Abstract(context.Background(), "ds", "doc.md"); err == nil {
		t.Fatal("expected error to bubble up")
	}
}

func TestCachedStore_DatasetAbstract_PropagatesError(t *testing.T) {
	cached := NewCachedStore(errStore{err: errors.New("boom")})
	if _, err := cached.DatasetAbstract(context.Background(), "ds"); err == nil {
		t.Fatal("expected error to bubble up")
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
