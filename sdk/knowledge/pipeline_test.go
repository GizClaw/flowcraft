package knowledge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// TestPipeline_AddDocumentDoesNotCallLLM is a regression test for the
// scheme E refactor: AddDocument must not synthesize layered context on
// its own. All LLM-driven summarization is now caller-controlled.
func TestPipeline_AddDocumentDoesNotCallLLM(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	if err := store.AddDocument(ctx, "ds", "doc.md", "body"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddDocuments(ctx, "ds", []DocInput{{Name: "doc2.md", Content: "more"}}); err != nil {
		t.Fatal(err)
	}

	// L0/L1 must remain empty since no caller produced them.
	abs, _ := store.Abstract(ctx, "ds", "doc.md")
	ov, _ := store.Overview(ctx, "ds", "doc.md")
	if abs != "" || ov != "" {
		t.Fatalf("expected empty L0/L1 after raw add; abs=%q ov=%q", abs, ov)
	}
	dsAbs, _ := store.DatasetAbstract(ctx, "ds")
	dsOv, _ := store.DatasetOverview(ctx, "ds")
	if dsAbs != "" || dsOv != "" {
		t.Fatalf("expected empty dataset L0/L1; abs=%q ov=%q", dsAbs, dsOv)
	}
}

// TestPipeline_GenerateAndPersistDocumentContext exercises the full
// caller-driven flow: generate → setter → sidecar, then verify reads
// (in-memory and from-disk via re-built index) return the new context.
func TestPipeline_GenerateAndPersistDocumentContext(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	stub := &stubLLM{abstractResp: "the abstract", overviewResp: "the overview"}

	if err := store.AddDocument(ctx, "ds", "doc.md", "body"); err != nil {
		t.Fatal(err)
	}

	docCtx, err := GenerateDocumentContext(ctx, stub, "body")
	if err != nil {
		t.Fatal(err)
	}

	// Apply to memory + disk.
	store.SetDocAbstract("ds", "doc.md", docCtx.Abstract)
	store.SetDocOverview("ds", "doc.md", docCtx.Overview)
	if err := store.WriteSidecar(ctx, "ds", "doc.md", ".abstract", docCtx.Abstract); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSidecar(ctx, "ds", "doc.md", ".overview", docCtx.Overview); err != nil {
		t.Fatal(err)
	}

	// In-memory reads.
	if got, _ := store.Abstract(ctx, "ds", "doc.md"); got != "the abstract" {
		t.Fatalf("abstract: got %q", got)
	}
	if got, _ := store.Overview(ctx, "ds", "doc.md"); got != "the overview" {
		t.Fatalf("overview: got %q", got)
	}

	// Drop in-memory state, rebuild from disk: sidecars should hydrate it.
	fresh := NewFSStore(ws)
	if err := fresh.BuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.Abstract(ctx, "ds", "doc.md"); got != "the abstract" {
		t.Fatalf("hydrated abstract: got %q", got)
	}
	if got, _ := fresh.Overview(ctx, "ds", "doc.md"); got != "the overview" {
		t.Fatalf("hydrated overview: got %q", got)
	}
}

// TestPipeline_GenerateAndPersistDatasetContext covers the dataset-level
// rollup variant: generate → setter → WriteDatasetFile, then re-hydrate.
func TestPipeline_GenerateAndPersistDatasetContext(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	stub := &stubLLM{abstractResp: "ds-l0", datasetOverviewResp: "ds-l1"}

	_ = store.AddDocument(ctx, "ds", "a.md", "alpha")
	_ = store.AddDocument(ctx, "ds", "b.md", "bravo")
	store.SetDocAbstract("ds", "a.md", "abs-a")
	store.SetDocAbstract("ds", "b.md", "abs-b")

	dsCtx, err := GenerateDatasetContext(ctx, stub, []DocumentSummary{
		{Name: "a.md", Abstract: "abs-a"},
		{Name: "b.md", Abstract: "abs-b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	store.SetDatasetAbstract("ds", dsCtx.Abstract)
	store.SetDatasetOverview("ds", dsCtx.Overview)
	if err := store.WriteDatasetFile(ctx, "ds", ".abstract.md", dsCtx.Abstract); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteDatasetFile(ctx, "ds", ".overview.md", dsCtx.Overview); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.DatasetAbstract(ctx, "ds"); got != "ds-l0" {
		t.Fatalf("dataset abstract: got %q", got)
	}
	if got, _ := store.DatasetOverview(ctx, "ds"); got != "ds-l1" {
		t.Fatalf("dataset overview: got %q", got)
	}

	// Re-hydrate from workspace.
	fresh := NewFSStore(ws)
	if err := fresh.BuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.DatasetAbstract(ctx, "ds"); got != "ds-l0" {
		t.Fatalf("hydrated dataset abstract: got %q", got)
	}
	if got, _ := fresh.DatasetOverview(ctx, "ds"); got != "ds-l1" {
		t.Fatalf("hydrated dataset overview: got %q", got)
	}
}

// TestPipeline_ConcurrentGenerateAndStore is a smoke test for thread safety
// when the caller fans out summarization across documents in parallel.
func TestPipeline_ConcurrentGenerateAndStore(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	stub := &countingLLM{resp: "ok"}

	docs := []string{"d1.md", "d2.md", "d3.md", "d4.md", "d5.md", "d6.md"}
	for _, name := range docs {
		if err := store.AddDocument(ctx, "ds", name, "content "+name); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for _, name := range docs {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c, err := GenerateDocumentContext(ctx, stub, "content "+name)
			if err != nil {
				t.Errorf("generate %s: %v", name, err)
				return
			}
			store.SetDocAbstract("ds", name, c.Abstract)
			store.SetDocOverview("ds", name, c.Overview)
			_ = store.WriteSidecar(ctx, "ds", name, ".abstract", c.Abstract)
			_ = store.WriteSidecar(ctx, "ds", name, ".overview", c.Overview)
		}(name)
	}
	wg.Wait()

	// Each doc should have the canned summary applied.
	for _, name := range docs {
		abs, _ := store.Abstract(ctx, "ds", name)
		if abs != "ok" {
			t.Fatalf("abstract for %s: got %q", name, abs)
		}
	}
	if got := stub.calls.Load(); got != int64(len(docs))*2 {
		t.Fatalf("expected %d LLM calls (2 per doc), got %d", len(docs)*2, got)
	}
}

// TestPipeline_GeneratorErrorLeavesStoreUntouched verifies that when the
// caller-driven pipeline errors out, the store is not silently mutated:
// it is the caller's responsibility (not the SDK's) to skip persistence.
func TestPipeline_GeneratorErrorLeavesStoreUntouched(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	stub := &stubLLM{abstractErr: errors.New("nope")}

	_ = store.AddDocument(ctx, "ds", "doc.md", "body")

	c, err := GenerateDocumentContext(ctx, stub, "body")
	if err == nil {
		t.Fatal("expected error")
	}
	// SDK contract: zero context returned, do not call setters.
	if c.Abstract != "" || c.Overview != "" {
		t.Fatalf("expected zero ctx, got %+v", c)
	}

	abs, _ := store.Abstract(ctx, "ds", "doc.md")
	ov, _ := store.Overview(ctx, "ds", "doc.md")
	if abs != "" || ov != "" {
		t.Fatalf("store should remain pristine; abs=%q ov=%q", abs, ov)
	}
}

// TestFSStore_WriteSidecar_AndDeleteCleansUp checks that WriteSidecar
// produces a file that DeleteDocument removes alongside the main doc.
func TestFSStore_WriteSidecar_AndDeleteCleansUp(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds", "doc.md", "body")
	if err := store.WriteSidecar(ctx, "ds", "doc.md", ".abstract", "L0"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSidecar(ctx, "ds", "doc.md", ".overview", "L1"); err != nil {
		t.Fatal(err)
	}

	// Sidecars should be on disk.
	exists, _ := ws.Exists(ctx, "knowledge/ds/doc.abstract")
	if !exists {
		t.Fatal("expected .abstract sidecar to exist")
	}

	if err := store.DeleteDocument(ctx, "ds", "doc.md"); err != nil {
		t.Fatal(err)
	}

	exists, _ = ws.Exists(ctx, "knowledge/ds/doc.abstract")
	if exists {
		t.Fatal("expected .abstract sidecar to be removed after DeleteDocument")
	}
	exists, _ = ws.Exists(ctx, "knowledge/ds/doc.overview")
	if exists {
		t.Fatal("expected .overview sidecar to be removed after DeleteDocument")
	}
}

// TestFSStore_WriteDatasetFile_Roundtrip checks that the dataset-level
// writer survives BuildIndex re-hydration.
func TestFSStore_WriteDatasetFile_Roundtrip(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	_ = store.AddDocument(ctx, "ds", "doc.md", "body")

	if err := store.WriteDatasetFile(ctx, "ds", ".abstract.md", "DS-ABS"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteDatasetFile(ctx, "ds", ".overview.md", "DS-OV"); err != nil {
		t.Fatal(err)
	}

	fresh := NewFSStore(ws)
	if err := fresh.BuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.DatasetAbstract(ctx, "ds"); got != "DS-ABS" {
		t.Fatalf("dataset abstract roundtrip: got %q", got)
	}
	if got, _ := fresh.DatasetOverview(ctx, "ds"); got != "DS-OV" {
		t.Fatalf("dataset overview roundtrip: got %q", got)
	}
}

// TestFSStore_DocAbstractStats_EvictsOnReplace exercises the abstractStats
// bookkeeping path inside SetDocAbstract: replacing an abstract should
// remove the old tokens before adding the new ones, otherwise repeated
// updates would inflate document frequencies and skew BM25 scores.
func TestFSStore_DocAbstractStats_EvictsOnReplace(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	_ = store.AddDocument(ctx, "ds", "doc.md", "body")

	store.SetDocAbstract("ds", "doc.md", "alpha alpha alpha")
	store.SetDocAbstract("ds", "doc.md", "beta")
	store.SetDocAbstract("ds", "doc.md", "")

	store.mu.RLock()
	di := store.index["ds"]
	df := di.abstractStats.DocFreq
	docCount := di.abstractStats.DocCount
	store.mu.RUnlock()

	if df["alpha"] != 0 {
		t.Fatalf("alpha postings should be evicted, got df=%d", df["alpha"])
	}
	if df["beta"] != 0 {
		t.Fatalf("beta postings should be evicted after clearing abstract, got df=%d", df["beta"])
	}
	if docCount != 0 {
		t.Fatalf("abstractStats DocCount should be zero after clearing, got %d", docCount)
	}
}

// TestFSStore_AbstractOverview_FallsBackToSidecarWhenNotIndexed verifies
// that Abstract/Overview can serve sidecar files even when the requested
// dataset/doc has not been pulled into the in-memory index yet (e.g. when
// a fresh FSStore is asked about a doc that was written by a previous
// process).
func TestFSStore_AbstractOverview_FallsBackToSidecarWhenNotIndexed(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds", "doc.md", "body")
	_ = store.WriteSidecar(ctx, "ds", "doc.md", ".abstract", "from-disk-abs")
	_ = store.WriteSidecar(ctx, "ds", "doc.md", ".overview", "from-disk-ov")

	// Fresh store, no in-memory index built yet.
	fresh := NewFSStore(ws)
	abs, err := fresh.Abstract(ctx, "ds", "doc.md")
	if err != nil {
		t.Fatalf("abstract: %v", err)
	}
	if abs != "from-disk-abs" {
		t.Fatalf("expected disk fallback, got %q", abs)
	}
	ov, err := fresh.Overview(ctx, "ds", "doc.md")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov != "from-disk-ov" {
		t.Fatalf("expected disk fallback, got %q", ov)
	}
}

// TestFSStore_SearchAcrossDatasets_LayerSelection drives the L0/L1
// branches of searchAcrossDatasets, which are exercised by the platform
// when datasetID is empty and max_layer is set.
func TestFSStore_SearchAcrossDatasets_LayerSelection(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds-go", "go.md", "Go programming language")
	store.SetDocAbstract("ds-go", "go.md", "Go is a compiled language for systems programming")
	store.SetDocOverview("ds-go", "go.md", "Go: types, concurrency, tooling, deployment")
	store.SetDatasetAbstract("ds-go", "Go ecosystem")

	_ = store.AddDocument(ctx, "ds-py", "py.md", "Python scripting language")
	store.SetDocAbstract("ds-py", "py.md", "Python is a dynamic language for scripting")
	store.SetDocOverview("ds-py", "py.md", "Python: typing, packaging, ecosystem")
	store.SetDatasetAbstract("ds-py", "Python ecosystem")

	t.Run("L0", func(t *testing.T) {
		results, err := store.Search(ctx, "", "Go programming", SearchOptions{MaxLayer: LayerAbstract, TopK: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) == 0 {
			t.Fatal("expected L0 cross-dataset results")
		}
		for _, r := range results {
			if r.Layer != LayerAbstract {
				t.Fatalf("expected L0, got %s", r.Layer)
			}
		}
	})

	t.Run("L1", func(t *testing.T) {
		results, err := store.Search(ctx, "", "concurrency tooling", SearchOptions{MaxLayer: LayerOverview, TopK: 5})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) == 0 {
			t.Fatal("expected L1 cross-dataset results")
		}
		for _, r := range results {
			if r.Layer != LayerOverview {
				t.Fatalf("expected L1, got %s", r.Layer)
			}
		}
	})
}

// TestFSStore_SearchDataset_LayerOverviewFallsBackToAbstract checks the
// branch where a doc has no L1 overview but does have L0 — searchDataset
// should still surface it under the L0 layer rather than skipping it.
func TestFSStore_SearchDataset_LayerOverviewFallsBackToAbstract(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	_ = store.AddDocument(ctx, "ds", "doc.md", "body")
	store.SetDocAbstract("ds", "doc.md", "Go is a programming language")

	results, err := store.Search(ctx, "ds", "programming", SearchOptions{MaxLayer: LayerOverview, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results from abstract fallback")
	}
	if results[0].Layer != LayerAbstract {
		t.Fatalf("expected L0 fallback layer, got %s", results[0].Layer)
	}
}

// TestFSStore_DocPath_AppendsMarkdownExt covers the branch where the
// caller omits the .md suffix.
func TestFSStore_DocPath_AppendsMarkdownExt(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws)
	ctx := context.Background()
	if err := store.AddDocument(ctx, "ds", "noext", "body"); err != nil {
		t.Fatal(err)
	}
	exists, _ := ws.Exists(ctx, "knowledge/ds/noext.md")
	if !exists {
		t.Fatal("expected .md suffix to be appended on disk")
	}
}

// TestCachedStore_AddDocument_InnerErrorDoesNotEvictCache verifies that a
// failing inner write does not flush the cache: if the platform retries
// later, cached reads remain valid until a successful write occurs.
func TestCachedStore_AddDocument_InnerErrorPropagates(t *testing.T) {
	cached := NewCachedStore(errStore{err: errors.New("boom")})
	if err := cached.AddDocument(context.Background(), "ds", "doc.md", "body"); err == nil {
		t.Fatal("expected error to surface from inner store")
	}
}

func TestCachedStore_DeleteDocument_InnerErrorPropagates(t *testing.T) {
	cached := NewCachedStore(errStore{err: errors.New("boom")})
	if err := cached.DeleteDocument(context.Background(), "ds", "doc.md"); err == nil {
		t.Fatal("expected error to surface from inner store")
	}
}

// --- helpers ---

// countingLLM returns a fixed response and counts invocations atomically.
type countingLLM struct {
	resp  string
	calls atomicInt64
}

func (c *countingLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	c.calls.Add(1)
	if len(msgs) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errors.New("empty")
	}
	// Sanity-check that prompts are well-formed strings.
	if !strings.Contains(msgs[0].Content(), "Document:") && !strings.Contains(msgs[0].Content(), "Document summaries:") {
		return llm.Message{}, llm.TokenUsage{}, errors.New("unexpected prompt shape")
	}
	return llm.NewTextMessage(llm.RoleAssistant, c.resp), llm.TokenUsage{}, nil
}

func (c *countingLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

// atomicInt64 is a minimal lock-free counter, avoiding a sync/atomic.Int64
// import shim across Go versions.
type atomicInt64 struct {
	mu sync.Mutex
	v  int64
}

func (a *atomicInt64) Add(n int64) {
	a.mu.Lock()
	a.v += n
	a.mu.Unlock()
}

func (a *atomicInt64) Load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}
