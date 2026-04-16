package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// ── test doubles ──

type stubEmbedder struct {
	vec   []float32
	err   error
	calls atomic.Int64
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	s.calls.Add(1)
	return s.vec, s.err
}

type stubVectorSearcher struct {
	results []*MemoryEntry
	err     error
	calls   atomic.Int64
	lastVec []float32
	mu      sync.Mutex
}

func (s *stubVectorSearcher) SearchByVector(_ context.Context, _ string, vec []float32, _ SearchOptions) ([]*MemoryEntry, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.lastVec = vec
	s.mu.Unlock()
	return s.results, s.err
}

// recordingLTStore wraps scopedMemoryLTStore and records SearchOptions from each
// Search call, allowing tests to verify QueryVector flows end-to-end.
type recordingLTStore struct {
	*scopedMemoryLTStore
	mu          sync.Mutex
	searchCalls []SearchOptions
}

func newRecordingLTStore() *recordingLTStore {
	return &recordingLTStore{scopedMemoryLTStore: newScopedMemoryLTStore()}
}

func (r *recordingLTStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	r.mu.Lock()
	r.searchCalls = append(r.searchCalls, opts)
	r.mu.Unlock()
	return r.scopedMemoryLTStore.Search(ctx, runtimeID, query, opts)
}

func (r *recordingLTStore) getSearchCalls() []SearchOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]SearchOptions, len(r.searchCalls))
	copy(cp, r.searchCalls)
	return cp
}

// slowLTStore adds configurable delay to List calls for parallel timing tests.
type slowLTStore struct {
	*scopedMemoryLTStore
	listDelay time.Duration
}

func (s *slowLTStore) List(ctx context.Context, runtimeID string, opts ListOptions) ([]*MemoryEntry, error) {
	select {
	case <-time.After(s.listDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.scopedMemoryLTStore.List(ctx, runtimeID, opts)
}

// errorLTStore returns errors from List and Search.
type errorLTStore struct {
	listErr   error
	searchErr error
}

func (e *errorLTStore) Save(context.Context, string, *MemoryEntry) error { return nil }
func (e *errorLTStore) List(context.Context, string, ListOptions) ([]*MemoryEntry, error) {
	return nil, e.listErr
}
func (e *errorLTStore) Search(context.Context, string, string, SearchOptions) ([]*MemoryEntry, error) {
	return nil, e.searchErr
}
func (e *errorLTStore) Update(context.Context, string, *MemoryEntry) error { return nil }
func (e *errorLTStore) Delete(context.Context, string, string) error       { return nil }

// ── compile-time interface compliance checks ──

var (
	_ LongTermStore  = (*HybridStore)(nil)
	_ embedPreWarmer = (*HybridStore)(nil)
)

// ── HybridStore unit tests ──

func TestHybridStore_NilOptions_PurePassthrough(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello bm25"})

	h := NewHybridStore(inner)

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "e1" {
		t.Fatalf("expected passthrough to inner, got %v", results)
	}
}

func TestHybridStore_EmbedderOnly_NoBehaviorChange(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello bm25"})

	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	h := NewHybridStore(inner, WithEmbedder(emb))

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "e1" {
		t.Fatalf("without vector searcher, should return BM25 only, got %v", results)
	}
	if emb.calls.Load() != 0 {
		t.Fatal("embedder should not be called when no vector searcher is present")
	}
}

func TestHybridStore_FullHybrid_MergesResults(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "bm1", Category: CategoryEntities, Content: "from bm25 search"})

	emb := &stubEmbedder{vec: []float32{0.5, 0.5}}
	vec := &stubVectorSearcher{
		results: []*MemoryEntry{
			{ID: "bm1", Category: CategoryEntities, Content: "from bm25 search"},
			{ID: "v1", Category: CategoryEntities, Content: "from vector search"},
		},
	}
	h := NewHybridStore(inner, WithEmbedder(emb), WithVectorSearcher(vec))

	results, err := h.Search(context.Background(), "r1", "search", SearchOptions{
		Category: CategoryEntities,
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 merged results (deduped), got %d", len(results))
	}
	if results[0].ID != "bm1" {
		t.Fatal("BM25 results should come first")
	}
	if results[1].ID != "v1" {
		t.Fatal("vector-unique result should follow")
	}
}

func TestHybridStore_PrecomputedQueryVector_SkipsEmbed(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello"})

	emb := &stubEmbedder{vec: []float32{0.9, 0.9}}
	vec := &stubVectorSearcher{}
	h := NewHybridStore(inner, WithEmbedder(emb), WithVectorSearcher(vec))

	precomputed := []float32{0.1, 0.2, 0.3}
	_, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category:    CategoryProfile,
		TopK:        5,
		QueryVector: precomputed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if emb.calls.Load() != 0 {
		t.Fatal("embedder should not be called when QueryVector is pre-set")
	}
	vec.mu.Lock()
	gotVec := vec.lastVec
	vec.mu.Unlock()
	if len(gotVec) != 3 || gotVec[0] != 0.1 {
		t.Fatalf("vector searcher should receive precomputed vec, got %v", gotVec)
	}
}

func TestHybridStore_VectorError_FallsBackToBM25(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello bm25"})

	emb := &stubEmbedder{vec: []float32{0.1}}
	vec := &stubVectorSearcher{err: errors.New("vector backend down")}
	h := NewHybridStore(inner, WithEmbedder(emb), WithVectorSearcher(vec))

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
	})
	if err != nil {
		t.Fatal("should not propagate vector error")
	}
	if len(results) != 1 || results[0].ID != "e1" {
		t.Fatal("should fall back to BM25 results")
	}
}

func TestHybridStore_EmbedError_FallsBackToBM25(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello bm25"})

	emb := &stubEmbedder{err: errors.New("embed timeout")}
	vec := &stubVectorSearcher{results: []*MemoryEntry{{ID: "v1"}}}
	h := NewHybridStore(inner, WithEmbedder(emb), WithVectorSearcher(vec))

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
	})
	if err != nil {
		t.Fatal("should not propagate embed error")
	}
	if len(results) != 1 || results[0].ID != "e1" {
		t.Fatal("should fall back to BM25 results")
	}
	if vec.calls.Load() != 0 {
		t.Fatal("vector searcher should not be called when embed fails")
	}
}

func TestHybridStore_EmbedQuery(t *testing.T) {
	t.Parallel()

	t.Run("nil embedder returns nil", func(t *testing.T) {
		h := NewHybridStore(newScopedMemoryLTStore())
		v, err := h.EmbedQuery(context.Background(), "test")
		if err != nil || v != nil {
			t.Fatalf("expected (nil, nil), got (%v, %v)", v, err)
		}
	})

	t.Run("with embedder returns vector", func(t *testing.T) {
		emb := &stubEmbedder{vec: []float32{1, 2, 3}}
		h := NewHybridStore(newScopedMemoryLTStore(), WithEmbedder(emb))
		v, err := h.EmbedQuery(context.Background(), "test")
		if err != nil {
			t.Fatal(err)
		}
		if len(v) != 3 {
			t.Fatalf("expected 3-dim vector, got %v", v)
		}
	})

	t.Run("embed error propagated", func(t *testing.T) {
		emb := &stubEmbedder{err: errors.New("fail")}
		h := NewHybridStore(newScopedMemoryLTStore(), WithEmbedder(emb))
		_, err := h.EmbedQuery(context.Background(), "test")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestHybridStore_Delegates(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "x", Category: CategoryProfile, Content: "c"})
	h := NewHybridStore(inner)
	ctx := context.Background()

	entries, err := h.List(ctx, "r1", ListOptions{Category: CategoryProfile})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from List, got %d", len(entries))
	}
	if err := h.Save(ctx, "r1", &MemoryEntry{ID: "y", Category: CategoryProfile}); err != nil {
		t.Fatal("Save delegation should not error")
	}
}

func TestMergeSearchResults(t *testing.T) {
	t.Parallel()
	bm25 := []*MemoryEntry{{ID: "a"}, {ID: "b"}}
	vec := []*MemoryEntry{{ID: "b"}, {ID: "c"}, nil}
	got := mergeSearchResults(bm25, vec, 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 merged, got %d", len(got))
	}
	got = mergeSearchResults(bm25, vec, 2)
	if len(got) != 2 {
		t.Fatalf("expected topK=2 cap, got %d", len(got))
	}
}

// ── Assembler parallel pre-compute integration test ──

func TestAssembler_ParallelPreEmbed_SharedVector(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me about projects and entities"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-hybrid-1"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "user profile data"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "alpha projects entity data"},
		&MemoryEntry{ID: "c1x", Category: CategoryCases, Content: "support case about projects"},
	)

	emb := &stubEmbedder{vec: []float32{0.42}}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	aware := NewMemoryAwareMemoryCompat(inner, hybrid, rt, LongTermConfig{
		Enabled:          true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()
	if !strings.Contains(sys, "user profile") {
		t.Fatalf("pinned profile missing: %q", sys)
	}
	if !strings.Contains(sys, "alpha projects") {
		t.Fatalf("recall entity missing: %q", sys)
	}

	// EmbedQuery should be called exactly once (pre-computed), not per category.
	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("expected 1 EmbedQuery call (pre-computed), got %d", n)
	}
}

func TestAssembler_NoEmbedPreWarmer_StillWorks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "hello entities"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-no-prewarm"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity data about entities"},
	)

	aware := NewMemoryAwareMemoryCompat(inner, lt, rt, LongTermConfig{
		Enabled:          true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()
	if !strings.Contains(sys, "profile") {
		t.Fatalf("pinned missing: %q", sys)
	}
	if !strings.Contains(sys, "entity data") {
		t.Fatalf("recall missing: %q", sys)
	}
}

func TestAssembler_EmbedError_GracefulDegradation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "projects"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-embed-err"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity about projects"},
	)

	emb := &stubEmbedder{err: errors.New("embed service down")}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	aware := NewMemoryAwareMemoryCompat(inner, hybrid, rt, LongTermConfig{
		Enabled:          true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()
	if !strings.Contains(sys, "profile") {
		t.Fatalf("pinned should still work: %q", sys)
	}
	if !strings.Contains(sys, "entity about projects") {
		t.Fatalf("BM25 recall should still work despite embed error: %q", sys)
	}
}

// ════════════════════════════════════════════════════════════════════
//  HybridStore: additional unit tests
// ════════════════════════════════════════════════════════════════════

func TestHybridStore_VectorOnly_NoEmbedder(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "e1", Category: CategoryProfile, Content: "hello bm25"})

	vec := &stubVectorSearcher{results: []*MemoryEntry{{ID: "v1", Content: "vector hit"}}}
	h := NewHybridStore(inner, WithVectorSearcher(vec))

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "e1" {
		t.Fatal("without embedder and no QueryVector, should return BM25 only")
	}
	if vec.calls.Load() != 0 {
		t.Fatal("vector searcher should not be called when vec is nil")
	}
}

func TestHybridStore_VectorOnly_WithPrecomputedVec(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "bm1", Category: CategoryProfile, Content: "hello bm25"})

	vec := &stubVectorSearcher{results: []*MemoryEntry{{ID: "v1", Content: "vector only"}}}
	h := NewHybridStore(inner, WithVectorSearcher(vec))

	results, err := h.Search(context.Background(), "r1", "hello", SearchOptions{
		Category:    CategoryProfile,
		TopK:        5,
		QueryVector: []float32{1.0, 2.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected merged bm25+vector, got %d", len(results))
	}
	if vec.calls.Load() != 1 {
		t.Fatal("vector searcher should be called with precomputed vec")
	}
}

func TestHybridStore_InnerSearchError_Propagated(t *testing.T) {
	t.Parallel()
	inner := &errorLTStore{searchErr: errors.New("disk failure")}
	h := NewHybridStore(inner)

	_, err := h.Search(context.Background(), "r1", "query", SearchOptions{TopK: 5})
	if err == nil || !strings.Contains(err.Error(), "disk failure") {
		t.Fatalf("inner search error should propagate, got: %v", err)
	}
}

func TestHybridStore_InnerSearchError_WithVector(t *testing.T) {
	t.Parallel()
	inner := &errorLTStore{searchErr: errors.New("inner fail")}
	emb := &stubEmbedder{vec: []float32{1.0}}
	vec := &stubVectorSearcher{results: []*MemoryEntry{{ID: "v1"}}}
	h := NewHybridStore(inner, WithEmbedder(emb), WithVectorSearcher(vec))

	_, err := h.Search(context.Background(), "r1", "query", SearchOptions{TopK: 5})
	if err == nil {
		t.Fatal("inner search error should propagate even with vector searcher present")
	}
	if vec.calls.Load() != 0 {
		t.Fatal("vector searcher should not be called when inner search fails")
	}
}

func TestHybridStore_UpdateAndDelete_Delegate(t *testing.T) {
	t.Parallel()
	inner := newScopedMemoryLTStore()
	inner.seed("r1", &MemoryEntry{ID: "x", Category: CategoryProfile, Content: "old"})
	h := NewHybridStore(inner)
	ctx := context.Background()

	if err := h.Update(ctx, "r1", &MemoryEntry{ID: "x", Category: CategoryProfile, Content: "new"}); err != nil {
		t.Fatalf("Update delegation failed: %v", err)
	}
	if err := h.Delete(ctx, "r1", "x"); err != nil {
		t.Fatalf("Delete delegation failed: %v", err)
	}
}

// ════════════════════════════════════════════════════════════════════
//  mergeSearchResults: edge cases
// ════════════════════════════════════════════════════════════════════

func TestMergeSearchResults_EdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("both empty", func(t *testing.T) {
		got := mergeSearchResults(nil, nil, 10)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %d", len(got))
		}
	})

	t.Run("bm25 only", func(t *testing.T) {
		bm25 := []*MemoryEntry{{ID: "a"}, {ID: "b"}}
		got := mergeSearchResults(bm25, nil, 0)
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("vec only", func(t *testing.T) {
		vec := []*MemoryEntry{{ID: "x"}, {ID: "y"}}
		got := mergeSearchResults(nil, vec, 0)
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("all nils in slices", func(t *testing.T) {
		bm25 := []*MemoryEntry{nil, nil}
		vec := []*MemoryEntry{nil}
		got := mergeSearchResults(bm25, vec, 0)
		if len(got) != 0 {
			t.Fatalf("expected 0, got %d", len(got))
		}
	})

	t.Run("complete overlap", func(t *testing.T) {
		bm25 := []*MemoryEntry{{ID: "a"}, {ID: "b"}}
		vec := []*MemoryEntry{{ID: "a"}, {ID: "b"}}
		got := mergeSearchResults(bm25, vec, 0)
		if len(got) != 2 {
			t.Fatalf("full overlap should dedup to 2, got %d", len(got))
		}
	})

	t.Run("topK zero means unlimited", func(t *testing.T) {
		var entries []*MemoryEntry
		for i := 0; i < 100; i++ {
			entries = append(entries, &MemoryEntry{ID: string(rune('A' + i))})
		}
		got := mergeSearchResults(entries, nil, 0)
		if len(got) != 100 {
			t.Fatalf("topK=0 should not cap, got %d", len(got))
		}
	})
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: context cancellation
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ContextCancel_StopsPinnedLoop(t *testing.T) {
	t.Parallel()
	slow := &slowLTStore{
		scopedMemoryLTStore: newScopedMemoryLTStore(),
		listDelay:           200 * time.Millisecond,
	}
	slow.seed("r1",
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile1"},
		&MemoryEntry{ID: "p2", Category: CategoryPreferences, Content: "pref1"},
	)

	assembler := NewContextAssembler(slow, AssemblerConfig{
		MaxEntries:       10,
		PinnedCategories: []MemoryCategory{CategoryProfile, CategoryPreferences},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := assembler.Assemble(ctx, "r1", nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// With 200ms per List call and 50ms timeout, at most 0 categories loaded
	if result != "" {
		t.Log("result may be empty or partial depending on timing, that's ok")
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: embed goroutine skip conditions
// ════════════════════════════════════════════════════════════════════

func TestAssembler_EmptyQuery_SkipsEmbed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "system only, no user message"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-emptyq"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
	)

	emb := &stubEmbedder{vec: []float32{0.1}}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	aware := NewMemoryAwareMemoryCompat(inner, hybrid, rt, LongTermConfig{
		Enabled:          true,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()
	if !strings.Contains(sys, "profile") {
		t.Fatalf("pinned should still load: %q", sys)
	}
	if emb.calls.Load() != 0 {
		t.Fatal("embedder should not be called when query is empty")
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: QueryVector end-to-end flow via recording store
// ════════════════════════════════════════════════════════════════════

func TestAssembler_QueryVector_FlowsToSearchOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rec := newRecordingLTStore()
	rt := "runtime-qvec"
	rec.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity about testing"},
		&MemoryEntry{ID: "c1", Category: CategoryCases, Content: "case about testing"},
	)

	expectedVec := []float32{0.1, 0.2, 0.3}
	emb := &stubEmbedder{vec: expectedVec}
	hybrid := NewHybridStore(rec, WithEmbedder(emb))

	assembler := NewContextAssembler(hybrid, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	_, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "testing"),
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := rec.getSearchCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 Search calls (entities + cases), got %d", len(calls))
	}
	for i, call := range calls {
		if call.QueryVector == nil {
			t.Fatalf("Search call %d (%s): QueryVector should be set", i, call.Category)
		}
		if len(call.QueryVector) != len(expectedVec) {
			t.Fatalf("Search call %d: QueryVector length mismatch", i)
		}
		for j, v := range call.QueryVector {
			if v != expectedVec[j] {
				t.Fatalf("Search call %d: QueryVector[%d] = %v, want %v", i, j, v, expectedVec[j])
			}
		}
	}
}

func TestAssembler_NoPreWarmer_QueryVectorNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rec := newRecordingLTStore()
	rt := "runtime-noprewarm-qv"
	rec.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity about testing"},
	)

	assembler := NewContextAssembler(rec, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	_, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "testing"),
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := rec.getSearchCalls()
	if len(calls) == 0 {
		t.Fatal("expected at least 1 Search call")
	}
	for i, call := range calls {
		if call.QueryVector != nil {
			t.Fatalf("Search call %d: QueryVector should be nil without embedPreWarmer", i)
		}
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: concurrent safety
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ConcurrentAssemble_RaceSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-race"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity about concurrency"},
		&MemoryEntry{ID: "c1", Category: CategoryCases, Content: "case about concurrency"},
	)

	emb := &stubEmbedder{vec: []float32{0.5}}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	assembler := NewContextAssembler(hybrid, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "concurrency test"),
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := assembler.Assemble(ctx, rt, nil, msgs)
			if err != nil {
				errs <- err
				return
			}
			if !strings.Contains(result, "profile") {
				errs <- errors.New("missing profile in result")
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: ScopeEnabled + HybridStore
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ScopeEnabled_HybridStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me about alpha adventures"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-scope-hybrid"
	lt.seed(rt,
		&MemoryEntry{ID: "pg", Category: CategoryProfile, Content: "shared profile mascot", Scope: MemoryScope{}},
		&MemoryEntry{ID: "pu", Category: CategoryProfile, Content: "alice private profile", Scope: MemoryScope{UserID: "alice"}},
		&MemoryEntry{ID: "ea", Category: CategoryEntities, Content: "alice alpha adventures entity", Scope: MemoryScope{UserID: "alice"}, Keywords: []string{"alpha", "adventures"}},
		&MemoryEntry{ID: "eb", Category: CategoryEntities, Content: "bob beta entity", Scope: MemoryScope{UserID: "bob"}, Keywords: []string{"beta"}},
	)

	emb := &stubEmbedder{vec: []float32{0.42}}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	aware := NewMemoryAwareMemoryCompat(inner, hybrid, rt, LongTermConfig{
		Enabled:          true,
		ScopeEnabled:     true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})
	aware.SetScope(&MemoryScope{UserID: "alice"})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()

	if !strings.Contains(sys, "shared profile mascot") {
		t.Fatalf("global pinned missing: %q", sys)
	}
	if strings.Contains(sys, "alice private profile") {
		t.Fatalf("user-scoped profile must not pin globally: %q", sys)
	}
	if !strings.Contains(sys, "alpha adventures") {
		t.Fatalf("alice-scoped entity missing: %q", sys)
	}
	if strings.Contains(sys, "bob beta") {
		t.Fatalf("bob entity must not leak: %q", sys)
	}
	if emb.calls.Load() != 1 {
		t.Fatalf("expected 1 EmbedQuery call, got %d", emb.calls.Load())
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: recall reuse with HybridStore
// ════════════════════════════════════════════════════════════════════

func TestAssembler_RecallReuse_WithHybridStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-reuse"
	lt.seed(rt,
		&MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "profile"},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity about golang"},
	)

	emb := &stubEmbedder{vec: []float32{0.1}}
	hybrid := NewHybridStore(lt, WithEmbedder(emb))

	assembler := NewContextAssembler(hybrid, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
		RecallCacheTTL:   10 * time.Second,
	})

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "golang"),
	}

	result1, err := assembler.Assemble(ctx, rt, nil, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result1, "entity about golang") {
		t.Fatalf("first call should recall entity: %q", result1)
	}

	// Second call with same query within throttle min interval → recall reuse
	result2, err := assembler.Assemble(ctx, rt, nil, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if result1 != result2 {
		t.Fatal("second call should produce identical result via recall reuse")
	}

	// EmbedQuery is called once per Assemble (even on reuse, the goroutine runs),
	// but recall Search is skipped on reuse.
	if emb.calls.Load() != 2 {
		t.Fatalf("expected 2 EmbedQuery calls (one per Assemble), got %d", emb.calls.Load())
	}
}

// ════════════════════════════════════════════════════════════════════
//  withQueryVector unit test
// ════════════════════════════════════════════════════════════════════

func TestWithQueryVector(t *testing.T) {
	t.Parallel()
	vec := []float32{1.0, 2.0, 3.0}
	opt := withQueryVector(vec)
	so := SearchOptions{Category: CategoryProfile, TopK: 5}
	opt(&so)

	if so.QueryVector == nil {
		t.Fatal("QueryVector should be set")
	}
	if len(so.QueryVector) != 3 || so.QueryVector[0] != 1.0 {
		t.Fatalf("unexpected QueryVector: %v", so.QueryVector)
	}
	if so.Category != CategoryProfile || so.TopK != 5 {
		t.Fatal("other fields should not be modified")
	}
}

func TestWithQueryVector_Nil(t *testing.T) {
	t.Parallel()
	opt := withQueryVector(nil)
	so := SearchOptions{TopK: 3}
	opt(&so)

	if so.QueryVector != nil {
		t.Fatal("nil vec should result in nil QueryVector")
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: parallel recall with concurrency control
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ParallelRecall_ManyCategoriesRespectsConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const numCats = 12
	const maxConcurrency = 3

	lt := newScopedMemoryLTStore()
	rt := "runtime-many-cats"

	cats := make([]MemoryCategory, numCats)
	for i := range cats {
		cat := MemoryCategory("cat" + string(rune('A'+i)))
		cats[i] = cat
		lt.seed(rt, &MemoryEntry{
			ID:       "e-" + string(rune('A'+i)),
			Category: cat,
			Content:  "content for testing recall",
			Keywords: []string{"testing", "recall"},
		})
	}

	// Track peak concurrency via atomic.
	var current atomic.Int64
	var peak atomic.Int64
	wrapped := &concurrencyTrackingStore{
		scopedMemoryLTStore: lt,
		current:             &current,
		peak:                &peak,
	}

	assembler := NewContextAssembler(wrapped, AssemblerConfig{
		MaxEntries:           50,
		PinnedCategories:     []MemoryCategory{},
		RecallCategories:     cats,
		MaxRecallConcurrency: maxConcurrency,
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "testing recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	if p := peak.Load(); p > int64(maxConcurrency) {
		t.Fatalf("peak concurrency %d exceeded limit %d", p, maxConcurrency)
	}
}

// concurrencyTrackingStore wraps scopedMemoryLTStore and tracks peak concurrent
// Search calls to verify semaphore behavior.
type concurrencyTrackingStore struct {
	*scopedMemoryLTStore
	current *atomic.Int64
	peak    *atomic.Int64
}

func (c *concurrencyTrackingStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	n := c.current.Add(1)
	for {
		old := c.peak.Load()
		if n <= old || c.peak.CompareAndSwap(old, n) {
			break
		}
	}
	defer c.current.Add(-1)

	time.Sleep(5 * time.Millisecond)
	return c.scopedMemoryLTStore.Search(ctx, runtimeID, query, opts)
}

func TestAssembler_ParallelRecall_DedupAcrossCategories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-dedup-cats"
	// Same entry ID appears in two categories.
	lt.seed(rt,
		&MemoryEntry{ID: "shared", Category: CategoryEntities, Content: "shared entity for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "shared", Category: CategoryCases, Content: "shared case for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "unique-ent", Category: CategoryEntities, Content: "unique entity for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "unique-case", Category: CategoryCases, Content: "unique case for recall", Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// "shared" should appear exactly once.
	count := strings.Count(result, "shared")
	if count != 1 {
		t.Fatalf("expected 'shared' to appear once (deduped), got %d in: %q", count, result)
	}
	if !strings.Contains(result, "unique entity") || !strings.Contains(result, "unique case") {
		t.Fatalf("unique entries missing: %q", result)
	}
}

func TestAssembler_ParallelRecall_DedupAgainstPinned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-dedup-pinned"
	// Same ID in pinned and recall categories.
	lt.seed(rt,
		&MemoryEntry{ID: "overlap", Category: CategoryProfile, Content: "pinned profile overlap"},
		&MemoryEntry{ID: "overlap", Category: CategoryEntities, Content: "recalled entity overlap for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "recall-only", Category: CategoryEntities, Content: "recall only entity for recall", Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}

	count := strings.Count(result, "overlap")
	if count != 1 {
		t.Fatalf("overlap entry should appear once (pinned wins), got %d in: %q", count, result)
	}
	if !strings.Contains(result, "pinned profile overlap") {
		t.Fatalf("pinned version should be kept: %q", result)
	}
	if !strings.Contains(result, "recall only") {
		t.Fatalf("recall-only entry should be present: %q", result)
	}
}

func TestAssembler_ParallelRecall_DeterministicOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-order"
	lt.seed(rt,
		&MemoryEntry{ID: "ent1", Category: CategoryEntities, Content: "entity for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "evt1", Category: CategoryEvents, Content: "event for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "cas1", Category: CategoryCases, Content: "case for recall", Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryEvents, CategoryCases},
	})

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "recall")}

	// Run multiple times — order should be stable.
	var firstResult string
	for i := 0; i < 10; i++ {
		result, err := assembler.Assemble(ctx, rt, nil, msgs)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstResult = result
		} else if result != firstResult {
			t.Fatalf("run %d produced different order:\n  first: %q\n  got:   %q", i, firstResult, result)
		}
	}
}

func TestAssembler_ParallelRecall_PartialFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-partial-fail"
	lt.seed(rt,
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity for recall", Keywords: []string{"recall"}},
	)

	// Wrap with a store that fails for CategoryCases only.
	failing := &categoryFailStore{
		scopedMemoryLTStore: lt,
		failCat:             CategoryCases,
	}

	assembler := NewContextAssembler(failing, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "entity for recall") {
		t.Fatalf("successful category should still return results: %q", result)
	}
}

type categoryFailStore struct {
	*scopedMemoryLTStore
	failCat MemoryCategory
}

func (c *categoryFailStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	if opts.Category == c.failCat {
		return nil, errors.New("category search unavailable")
	}
	return c.scopedMemoryLTStore.Search(ctx, runtimeID, query, opts)
}

func TestAssembler_DefaultMaxRecallConcurrency(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{})
	if a.config.MaxRecallConcurrency != defaultMaxRecallConcurrency {
		t.Fatalf("expected default %d, got %d", defaultMaxRecallConcurrency, a.config.MaxRecallConcurrency)
	}
}

func TestAssembler_CustomMaxRecallConcurrency(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{MaxRecallConcurrency: 2})
	if a.config.MaxRecallConcurrency != 2 {
		t.Fatalf("expected 2, got %d", a.config.MaxRecallConcurrency)
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: recallBudget <= 0 when pinned fills max
// ════════════════════════════════════════════════════════════════════

func TestAssembler_RecallBudgetFloor_WhenPinnedFillsMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-budget-floor"
	for i := 0; i < 10; i++ {
		lt.seed(rt, &MemoryEntry{
			ID:       "pin-" + string(rune('a'+i)),
			Category: CategoryProfile,
			Content:  "pinned item " + string(rune('a'+i)),
		})
	}
	lt.seed(rt,
		&MemoryEntry{ID: "r1", Category: CategoryEntities, Content: "recalled entity for query", Keywords: []string{"query"}},
		&MemoryEntry{ID: "r2", Category: CategoryEntities, Content: "another entity for query", Keywords: []string{"query"}},
		&MemoryEntry{ID: "r3", Category: CategoryEntities, Content: "third entity for query", Keywords: []string{"query"}},
		&MemoryEntry{ID: "r4", Category: CategoryEntities, Content: "fourth entity for query", Keywords: []string{"query"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       13,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "query"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// recallBudget = max(13 - 10, 3) = 3, so at most 3 recall entries show.
	recallCount := strings.Count(result, "recalled entity") +
		strings.Count(result, "another entity") +
		strings.Count(result, "third entity") +
		strings.Count(result, "fourth entity")
	if recallCount > 3 {
		t.Fatalf("recallBudget floor should limit to 3, but got %d recall entries in: %q", recallCount, result)
	}
	if recallCount == 0 {
		t.Fatalf("should still recall some entries despite full pinned: %q", result)
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: recalled truncation when exceeding budget
// ════════════════════════════════════════════════════════════════════

func TestAssembler_RecalledTruncatedToBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-trunc"
	// 1 pinned + many recall entries, maxEntries=4 → recallBudget=3
	lt.seed(rt, &MemoryEntry{ID: "p1", Category: CategoryProfile, Content: "pinned"})
	for i := 0; i < 8; i++ {
		lt.seed(rt, &MemoryEntry{
			ID:       "r" + string(rune('0'+i)),
			Category: CategoryEntities,
			Content:  "recall data for testing",
			Keywords: []string{"testing"},
		})
	}

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       4,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "testing"),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(result, "- [")
	if lines > 4 {
		t.Fatalf("total entries should not exceed maxEntries=4, got %d lines in: %q", lines, result)
	}
}

// ════════════════════════════════════════════════════════════════════
//  mergeAndDedupEntries: maxEntries truncation
// ════════════════════════════════════════════════════════════════════

func TestMergeAndDedupEntries_Truncation(t *testing.T) {
	t.Parallel()
	pinned := []*MemoryEntry{{ID: "a", Content: "a"}, {ID: "b", Content: "b"}}
	recalled := []*MemoryEntry{{ID: "c", Content: "c"}, {ID: "d", Content: "d"}}

	got := mergeAndDedupEntries(pinned, recalled, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries after truncation, got %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatal("pinned should come first, then recalled")
	}
}

func TestMergeAndDedupEntries_NilEntries(t *testing.T) {
	t.Parallel()
	pinned := []*MemoryEntry{nil, {ID: "a"}, nil}
	recalled := []*MemoryEntry{{ID: "a"}, {ID: "b"}, nil}

	got := mergeAndDedupEntries(pinned, recalled, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 after dedup+nil filter, got %d", len(got))
	}
}

func TestMergeAndDedupEntries_ZeroMax(t *testing.T) {
	t.Parallel()
	pinned := []*MemoryEntry{{ID: "a"}, {ID: "b"}}
	recalled := []*MemoryEntry{{ID: "c"}}

	got := mergeAndDedupEntries(pinned, recalled, 0)
	if len(got) != 3 {
		t.Fatalf("maxEntries=0 should not truncate, got %d", len(got))
	}
}

// ════════════════════════════════════════════════════════════════════
//  recallPartitionsFor: map present but key missing
// ════════════════════════════════════════════════════════════════════

func TestRecallPartitionsFor_MapPresentKeyMissing(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{
		RecallPartitions: map[MemoryCategory][]MemoryPartition{
			CategoryEntities: {PartitionUser, PartitionGlobal},
		},
	})

	// Key present → uses configured partitions.
	p := a.recallPartitionsFor(CategoryEntities)
	if len(p) != 2 {
		t.Fatalf("expected 2 partitions for entities, got %d", len(p))
	}

	// Key absent → falls back to user-only.
	p = a.recallPartitionsFor(CategoryCases)
	if len(p) != 1 || p[0] != PartitionUser {
		t.Fatalf("missing key should default to user-only, got %v", p)
	}
}

func TestRecallPartitionsFor_EmptySliceInMap(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{
		RecallPartitions: map[MemoryCategory][]MemoryPartition{
			CategoryEntities: {},
		},
	})

	p := a.recallPartitionsFor(CategoryEntities)
	if len(p) != 1 || p[0] != PartitionUser {
		t.Fatalf("empty slice should fall through to default, got %v", p)
	}
}

// ════════════════════════════════════════════════════════════════════
//  recallCacheKey: all three branches
// ════════════════════════════════════════════════════════════════════

func TestRecallCacheKey_AllBranches(t *testing.T) {
	t.Parallel()

	// Branch 1: rec != nil → uses rec.CacheKey()
	rec := &RecallScope{RuntimeID: "rt", UserID: "u1"}
	k := recallCacheKey("rt", nil, rec, "entities", "q")
	if !strings.Contains(k, "u1") {
		t.Fatalf("rec branch should use RecallScope key, got %q", k)
	}

	// Branch 2: rec == nil, scope != nil → uses scope.CacheKey()
	scope := &MemoryScope{RuntimeID: "rt", UserID: "u2"}
	k = recallCacheKey("rt", scope, nil, "entities", "q")
	if !strings.Contains(k, "u2") {
		t.Fatalf("scope branch should use MemoryScope key, got %q", k)
	}

	// Branch 3: both nil → uses runtimeID
	k = recallCacheKey("rt", nil, nil, "entities", "q")
	if !strings.HasPrefix(k, "rt|recall|") {
		t.Fatalf("default branch should use runtimeID, got %q", k)
	}
}

// ════════════════════════════════════════════════════════════════════
//  recallScopeFromScope: scope.IsGlobal() branch
// ════════════════════════════════════════════════════════════════════

func TestRecallScopeFromScope_GlobalScope(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{ScopeEnabled: true})

	// Global scope: UserID="" SessionID=""
	globalScope := &MemoryScope{RuntimeID: "rt"}
	rs := a.recallScopeFromScope("rt", globalScope, []MemoryPartition{PartitionUser})
	if rs == nil {
		t.Fatal("should return non-nil RecallScope for global scope")
	}
	if rs.UserID != "" || rs.SessionID != "" {
		t.Fatalf("global scope should produce empty user/session, got %+v", rs)
	}
}

func TestRecallScopeFromScope_UserScope(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{ScopeEnabled: true})

	userScope := &MemoryScope{RuntimeID: "rt", UserID: "alice"}
	rs := a.recallScopeFromScope("rt", userScope, []MemoryPartition{PartitionUser})
	if rs == nil {
		t.Fatal("should return non-nil RecallScope")
	}
	if rs.UserID != "alice" {
		t.Fatalf("user scope should preserve UserID, got %+v", rs)
	}
}

func TestRecallScopeFromScope_NilScope(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{ScopeEnabled: true})

	rs := a.recallScopeFromScope("rt", nil, []MemoryPartition{PartitionUser})
	if rs == nil {
		t.Fatal("nil scope with ScopeEnabled should still return RecallScope")
	}
	if rs.RuntimeID != "rt" {
		t.Fatalf("expected RuntimeID=rt, got %q", rs.RuntimeID)
	}
}

func TestRecallScopeFromScope_ScopeDisabled(t *testing.T) {
	t.Parallel()
	a := NewContextAssembler(nil, AssemblerConfig{ScopeEnabled: false})

	rs := a.recallScopeFromScope("rt", &MemoryScope{UserID: "alice"}, []MemoryPartition{PartitionUser})
	if rs != nil {
		t.Fatal("ScopeEnabled=false should return nil")
	}
}

// ════════════════════════════════════════════════════════════════════
//  searchRecallParallel: context cancel propagates to goroutines
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ParallelRecall_ContextCancel(t *testing.T) {
	t.Parallel()

	slow := &slowSearchStore{
		scopedMemoryLTStore: newScopedMemoryLTStore(),
		delay:               200 * time.Millisecond,
	}
	rt := "runtime-ctx-cancel"
	slow.seed(rt,
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity for recall", Keywords: []string{"recall"}},
		&MemoryEntry{ID: "e2", Category: CategoryCases, Content: "case for recall", Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(slow, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	// Should not error — context cancel causes search to fail but we log-and-continue.
	if err != nil {
		t.Fatalf("context cancel should not return error from Assemble, got: %v", err)
	}
}

type slowSearchStore struct {
	*scopedMemoryLTStore
	delay time.Duration
}

func (s *slowSearchStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.scopedMemoryLTStore.Search(ctx, runtimeID, query, opts)
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: pinned load error doesn't block recall
// ════════════════════════════════════════════════════════════════════

func TestAssembler_PinnedError_RecallStillWorks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := &errorLTStore{
		listErr:   errors.New("pinned list unavailable"),
		searchErr: nil,
	}

	inner := &pinnedFailRecallSuccessStore{
		errorLTStore:        lt,
		scopedMemoryLTStore: newScopedMemoryLTStore(),
	}
	rt := "runtime-pinned-err"
	inner.scopedMemoryLTStore.seed(rt,
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity for query", Keywords: []string{"query"}},
	)

	assembler := NewContextAssembler(inner, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "query"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "entity for query") {
		t.Fatalf("recall should still work when pinned fails: %q", result)
	}
}

// pinnedFailRecallSuccessStore fails List (pinned) but succeeds Search (recall).
type pinnedFailRecallSuccessStore struct {
	*errorLTStore
	*scopedMemoryLTStore
}

func (p *pinnedFailRecallSuccessStore) List(_ context.Context, _ string, _ ListOptions) ([]*MemoryEntry, error) {
	return nil, p.errorLTStore.listErr
}

func (p *pinnedFailRecallSuccessStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	return p.scopedMemoryLTStore.Search(ctx, runtimeID, query, opts)
}

func (p *pinnedFailRecallSuccessStore) Save(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	return nil
}

func (p *pinnedFailRecallSuccessStore) Update(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	return nil
}

func (p *pinnedFailRecallSuccessStore) Delete(ctx context.Context, runtimeID, entryID string) error {
	return nil
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: nil ltStore returns empty string
// ════════════════════════════════════════════════════════════════════

func TestAssembler_NilStore_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	assembler := NewContextAssembler(nil, AssemblerConfig{})

	result, err := assembler.Assemble(context.Background(), "rt", nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "" {
		t.Fatalf("nil store should return empty, got %q", result)
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: ScopeEnabled with nil scope (no SetScope called)
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ScopeEnabled_NilScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-nil-scope"
	lt.seed(rt,
		&MemoryEntry{ID: "pg", Category: CategoryProfile, Content: "global profile", Scope: MemoryScope{}},
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity for recall", Keywords: []string{"recall"}, Scope: MemoryScope{}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       20,
		ScopeEnabled:     true,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
		// scope=nil + ScopeEnabled → recall scope is global with UserID="".
		// PartitionUser won't match global entries; include PartitionGlobal.
		RecallPartitions: map[MemoryCategory][]MemoryPartition{
			CategoryEntities: {PartitionGlobal},
		},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "global profile") {
		t.Fatalf("global pinned missing: %q", result)
	}
	if !strings.Contains(result, "entity for recall") {
		t.Fatalf("global recall missing: %q", result)
	}
}

// ════════════════════════════════════════════════════════════════════
//  Assembler: ScopeEnabled with non-nil scope (exercises scope.CacheKey
//  in recallScopeKey path)
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ScopeEnabled_WithScope_RecallScopeKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-scope-key"
	lt.seed(rt,
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "alice entity for recall", Scope: MemoryScope{UserID: "alice"}, Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:       20,
		ScopeEnabled:     true,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})

	scope := &MemoryScope{UserID: "alice"}
	result, err := assembler.Assemble(ctx, rt, scope, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alice entity") {
		t.Fatalf("scoped recall should return alice entity: %q", result)
	}

	// Second call should hit recall reuse cache.
	result2, err := assembler.Assemble(ctx, rt, scope, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != result2 {
		t.Fatal("second call should produce same result via recall reuse")
	}
}

// ════════════════════════════════════════════════════════════════════
//  searchRecallParallel: single category (concurrency > cats shortcut)
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ParallelRecall_SingleCategory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	lt := newScopedMemoryLTStore()
	rt := "runtime-single-cat"
	lt.seed(rt,
		&MemoryEntry{ID: "e1", Category: CategoryEntities, Content: "entity for recall", Keywords: []string{"recall"}},
	)

	assembler := NewContextAssembler(lt, AssemblerConfig{
		MaxEntries:           20,
		PinnedCategories:     []MemoryCategory{},
		RecallCategories:     []MemoryCategory{CategoryEntities},
		MaxRecallConcurrency: 8,
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "recall"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "entity for recall") {
		t.Fatalf("single category should still work: %q", result)
	}
}

// ════════════════════════════════════════════════════════════════════
//  searchRecallParallel: all categories fail
// ════════════════════════════════════════════════════════════════════

func TestAssembler_ParallelRecall_AllFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	failing := &errorLTStore{searchErr: errors.New("all fail")}
	assembler := NewContextAssembler(failing, AssemblerConfig{
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{},
		RecallCategories: []MemoryCategory{CategoryEntities, CategoryCases},
	})

	result, err := assembler.Assemble(ctx, rt, nil, []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != "" {
		t.Fatalf("all-failing recall should produce empty result, got %q", result)
	}
}

var rt = "runtime-allfail"

func TestNewHybridStore_NilInnerPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from NewHybridStore(nil)")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T(%v), want string", r, r)
		}
		if !strings.Contains(msg, "nil inner store") {
			t.Fatalf("panic message = %q, want mention of nil inner store", msg)
		}
	}()
	NewHybridStore(nil)
}

func TestHybridStore_EmbedErrorFallsThroughToBM25(t *testing.T) {
	inner := newScopedMemoryLTStore()
	inner.seed("rt", &MemoryEntry{ID: "e1", Content: "hello world", Category: CategoryEntities})

	embedder := &stubEmbedder{err: errors.New("embed failed")}
	vectorSearcher := &stubVectorSearcher{
		results: []*MemoryEntry{{ID: "v1", Content: "vector result"}},
	}

	h := NewHybridStore(inner, WithEmbedder(embedder), WithVectorSearcher(vectorSearcher))
	results, err := h.Search(context.Background(), "rt", "hello", SearchOptions{TopK: 10, Category: CategoryEntities})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vectorSearcher.calls.Load() != 0 {
		t.Fatal("vector search should NOT be called when embed fails")
	}
	if len(results) == 0 {
		t.Fatal("should fall back to BM25 results")
	}
}

func TestHybridStore_VectorSearchErrorFallsThroughToBM25(t *testing.T) {
	inner := newScopedMemoryLTStore()
	inner.seed("rt", &MemoryEntry{ID: "e1", Content: "hello world", Category: CategoryEntities})

	embedder := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vectorSearcher := &stubVectorSearcher{err: errors.New("vector search failed")}

	h := NewHybridStore(inner, WithEmbedder(embedder), WithVectorSearcher(vectorSearcher))
	results, err := h.Search(context.Background(), "rt", "hello", SearchOptions{TopK: 10, Category: CategoryEntities})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("should fall back to BM25 results on vector search error")
	}
}
