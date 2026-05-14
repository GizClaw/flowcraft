package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	rt "github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func newChunkRepo(t *testing.T) *RetrievalChunkRepo {
	t.Helper()
	return NewChunkRepo(memory.New())
}

// newChunkRepoWithIndex returns both the repo and the underlying
// in-memory index so tests can inspect the doc-level namespace
// directly without going through SearchDocs.
func newChunkRepoWithIndex(t *testing.T) (*RetrievalChunkRepo, *memory.Index) {
	t.Helper()
	idx := memory.New()
	return NewChunkRepo(idx), idx
}

// listDocsNamespace dumps every retrieval.Doc currently held in the
// dataset's __docs namespace; used to assert doc-level write
// behaviour independently of the SearchDocs query path.
func listDocsNamespace(t *testing.T, idx *memory.Index, datasetID string) []rt.Doc {
	t.Helper()
	resp, err := idx.List(context.Background(), docsNamespace(datasetID), rt.ListRequest{PageSize: 100})
	if err != nil {
		t.Fatalf("list docs namespace: %v", err)
	}
	if resp == nil {
		return nil
	}
	return resp.Items
}

func chunk(doc string, idx int, content string) knowledge.DerivedChunk {
	return knowledge.DerivedChunk{
		DocName: doc,
		Index:   idx,
		Content: content,
		Sig:     knowledge.DerivedSig{SourceVer: 7, ChunkerSig: "chunker:v1"},
	}
}

func TestRetrievalChunkRepo_NamespacePerDataset(t *testing.T) {
	t.Parallel()
	if got, want := chunksNamespace("ds-1"), "kb_ds_1__chunks"; got != want {
		t.Fatalf("namespace = %q, want %q", got, want)
	}
}

func TestRetrievalChunkRepo_ReplaceEliminatesStaleChunks(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "doc.md", []knowledge.DerivedChunk{
		chunk("doc.md", 0, "alpha beta"),
		chunk("doc.md", 1, "gamma delta"),
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := r.Replace(ctx, "ds", "doc.md", []knowledge.DerivedChunk{
		chunk("doc.md", 0, "epsilon zeta"),
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "epsilon",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search after replace: %v", err)
	}
	bodies := map[string]bool{}
	for _, c := range cands {
		bodies[c.Hit.Content] = true
	}
	if bodies["alpha beta"] || bodies["gamma delta"] {
		t.Fatalf("stale chunk surfaced after replace: %+v", cands)
	}
	if !bodies["epsilon zeta"] {
		t.Fatalf("new chunk not present: %+v", cands)
	}
}

func TestRetrievalChunkRepo_DeleteByDoc(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace a: %v", err)
	}
	if err := r.Replace(ctx, "ds", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "beta")}); err != nil {
		t.Fatalf("replace b: %v", err)
	}
	if err := r.DeleteByDoc(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "beta", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	for _, c := range cands {
		if c.Hit.DocName == "a.md" {
			t.Fatalf("a.md should be gone, surfaced: %+v", c.Hit)
		}
	}
	if len(cands) == 0 {
		t.Fatalf("b.md missing")
	}
}

func TestRetrievalChunkRepo_DeleteByDataset(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds"); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected no results after drop, got %+v", cands)
	}
}

func TestRetrievalChunkRepo_FanOutAcrossDatasets(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds1", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "shared keyword apple")}); err != nil {
		t.Fatalf("replace ds1: %v", err)
	}
	if err := r.Replace(ctx, "ds2", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "shared keyword banana")}); err != nil {
		t.Fatalf("replace ds2: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds1", "ds2"},
		Text:       "shared",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		seen[c.Hit.DatasetID] = true
	}
	if !seen["ds1"] || !seen["ds2"] {
		t.Fatalf("fan-out missed a dataset: seen=%v", seen)
	}
}

func TestRetrievalChunkRepo_EmptyDatasetIDsReturnsNil(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search empty datasets: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("empty datasets should return nothing, got %+v", cands)
	}
}

func TestRetrievalChunkRepo_VectorMode(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	c1 := chunk("a.md", 0, "apple")
	c1.Vector = []float32{1, 0, 0}
	c2 := chunk("a.md", 1, "banana")
	c2.Vector = []float32{0, 1, 0}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{c1, c2}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeVector,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) == 0 {
		t.Fatalf("vector search returned no candidates")
	}
	if cands[0].Hit.ChunkIndex != 0 {
		t.Fatalf("top vector hit = idx %d, want 0", cands[0].Hit.ChunkIndex)
	}
	if cands[0].Source != "vector" {
		t.Fatalf("source = %q, want vector", cands[0].Source)
	}
}

func TestRetrievalChunkRepo_HybridFansOutBothLanes(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	c := chunk("a.md", 0, "apple sauce")
	c.Vector = []float32{0.5, 0.5}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{c}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "apple",
		Vector:     []float32{0.5, 0.5},
		Mode:       knowledge.ModeHybrid,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	bm25 := 0
	vec := 0
	for _, c := range cands {
		switch c.Source {
		case "bm25":
			bm25++
		case "vector":
			vec++
		}
	}
	if bm25 == 0 || vec == 0 {
		t.Fatalf("hybrid did not fan out (bm25=%d vector=%d)", bm25, vec)
	}
}

func TestRetrievalChunkRepo_NamespaceIsolation(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds1", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace ds1: %v", err)
	}
	if err := r.Replace(ctx, "ds2", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace ds2: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds1"); err != nil {
		t.Fatalf("delete ds1: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds2"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search ds2: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("ds2 lost data after dropping ds1: %+v", cands)
	}
}

func TestRetrievalChunkRepo_SearchDocs_ImplementsDocLevelSearcher(t *testing.T) {
	r := newChunkRepo(t)
	if _, ok := any(r).(knowledge.DocLevelSearcher); !ok {
		t.Fatalf("RetrievalChunkRepo must implement knowledge.DocLevelSearcher")
	}
}

func TestRetrievalChunkRepo_SearchDocs_OneHitPerDoc(t *testing.T) {
	// A query whose keyword appears in N chunks of the same doc must
	// still produce exactly ONE doc-level hit for that doc — the
	// whole point of doc-level collapse.
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "alpha"),
		chunk("a.md", 1, "alpha"),
		chunk("a.md", 2, "alpha"),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "alpha",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 doc-level hit, got %d (cands=%+v)", len(cands), cands)
	}
	if cands[0].Hit.DocName != "a.md" {
		t.Fatalf("docName = %q, want a.md", cands[0].Hit.DocName)
	}
	if cands[0].Hit.ChunkIndex != -1 {
		t.Fatalf("doc-level hit has ChunkIndex %d, want -1", cands[0].Hit.ChunkIndex)
	}
	if cands[0].Hit.Layer != "" {
		t.Fatalf("doc-level hit has Layer %q, want \"\"", cands[0].Hit.Layer)
	}
}

func TestRetrievalChunkRepo_SearchDocs_SumPoolAggregatesAcrossChunks(t *testing.T) {
	// A doc whose query keywords are SPLIT across two chunks must
	// outrank a non-relevant doc that only matches one of those
	// keywords (even repeatedly) in a single chunk. This is the exact
	// failure mode of max-pool chunk-collapse documented in #126;
	// sum-pool folds the per-chunk evidence into one doc score.
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "rel.md", []knowledge.DerivedChunk{
		chunk("rel.md", 0, "alpha bravo charlie"),
		chunk("rel.md", 1, "delta echo foxtrot"),
	}); err != nil {
		t.Fatalf("replace rel: %v", err)
	}
	if err := r.Replace(ctx, "ds", "noise.md", []knowledge.DerivedChunk{
		chunk("noise.md", 0, "alpha alpha alpha zulu"),
	}); err != nil {
		t.Fatalf("replace noise: %v", err)
	}

	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "alpha foxtrot",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) < 2 {
		t.Fatalf("expected both docs, got %d", len(cands))
	}
	if cands[0].Hit.DocName != "rel.md" {
		t.Fatalf("top doc = %q, want rel.md (sum-pool should let two-keyword coverage beat one-keyword repetition); ranking = %+v",
			cands[0].Hit.DocName, cands)
	}
}

func TestRetrievalChunkRepo_SearchDocs_RebuildsOnReplace(t *testing.T) {
	// Doc-level results derive from the chunk index; a Replace that
	// rewrites a doc's chunks must immediately surface the new tokens
	// and stop surfacing the old ones — no stale cache layer.
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "alpha"),
	}); err != nil {
		t.Fatalf("replace v1: %v", err)
	}
	cands, _ := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if len(cands) != 1 {
		t.Fatalf("pre-replace: got %d, want 1", len(cands))
	}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "epsilon"),
	}); err != nil {
		t.Fatalf("replace v2: %v", err)
	}
	cands, _ = r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if len(cands) != 0 {
		t.Fatalf("post-replace stale: %+v", cands)
	}
	cands, _ = r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"}, Text: "epsilon", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if len(cands) != 1 {
		t.Fatalf("post-replace new term not found: %+v", cands)
	}
}

func TestRetrievalChunkRepo_SearchDocs_RespectsTopK(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		name := "doc" + itoa(i) + ".md"
		if err := r.Replace(ctx, "ds", name, []knowledge.DerivedChunk{
			chunk(name, 0, "shared keyword body "+name),
		}); err != nil {
			t.Fatalf("replace %s: %v", name, err)
		}
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "shared",
		Mode:       knowledge.ModeBM25,
		TopK:       3,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("topK=3 returned %d hits", len(cands))
	}
}

func TestRetrievalChunkRepo_SearchDocs_FanOutAcrossDatasets(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds1", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "shared apple")}); err != nil {
		t.Fatalf("replace ds1: %v", err)
	}
	if err := r.Replace(ctx, "ds2", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "shared banana")}); err != nil {
		t.Fatalf("replace ds2: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds1", "ds2"},
		Text:       "shared",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		seen[c.Hit.DatasetID] = true
	}
	if !seen["ds1"] || !seen["ds2"] {
		t.Fatalf("doc-level fan-out missed a dataset: seen=%v", seen)
	}
}

func TestRetrievalChunkRepo_SearchDocs_HybridContributesBothLanes(t *testing.T) {
	// Hybrid mode should let both BM25 and vector signal contribute
	// to the doc's aggregate score.
	r := newChunkRepo(t)
	ctx := context.Background()
	c := chunk("a.md", 0, "apple")
	c.Vector = []float32{1, 0, 0}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{c}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "apple",
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeHybrid,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 doc-level hit, got %d", len(cands))
	}
	if cands[0].Hit.Score <= 0 {
		t.Fatalf("hybrid doc-level score = %v, expected > 0 (both lanes should contribute)", cands[0].Hit.Score)
	}
}

func TestRetrievalChunkRepo_SearchDocs_SourceLabelsCrossLaneAggregateAsSumpool(t *testing.T) {
	// When a doc's matching chunks span more than one lane
	// (hybrid bm25 + vector for the same chunk → two candidates),
	// the aggregated doc Candidate must carry sumpoolSource rather
	// than whichever lane was encountered first. Downstream
	// lane-aware code (fusion / labelling) would otherwise be
	// silently misled by iteration order.
	r := newChunkRepo(t)
	ctx := context.Background()
	c := chunk("a.md", 0, "apple")
	c.Vector = []float32{1, 0, 0}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{c}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "apple",
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeHybrid,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 doc-level hit, got %d", len(cands))
	}
	if got := cands[0].Source; got != "sumpool" {
		t.Fatalf("cross-lane aggregate Source = %q, want \"sumpool\"", got)
	}
}

func TestRetrievalChunkRepo_SearchDocs_SourceLabelsSingleLanePreserved(t *testing.T) {
	// When every contributing chunk shares one lane (BM25-only),
	// the aggregate must keep that lane's label rather than
	// promoting to sumpoolSource. Lane-aware downstream code on the
	// BM25-only eval path (e.g. BEIR scifact lanes=bm25) depends on
	// this.
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "apple"),
		chunk("a.md", 1, "apple banana"),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "apple",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 doc-level hit, got %d", len(cands))
	}
	if got := cands[0].Source; got != "bm25" {
		t.Fatalf("single-lane aggregate Source = %q, want \"bm25\"", got)
	}
}

func TestRetrievalChunkRepo_SearchDocs_NoResults(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "no-such-term",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected 0 hits for non-matching query, got %+v", cands)
	}
}

// --- __docs namespace cascade (#134) ---------------------------------------

func TestRetrievalChunkRepo_Replace_PopulatesDocsNamespace(t *testing.T) {
	// Replace must seed the doc-level namespace with one Doc per
	// logical document, ID = docName, Content = concatenation of
	// chunk contents in chunk-index order.
	r, idx := newChunkRepoWithIndex(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 1, "beta gamma"),
		chunk("a.md", 0, "alpha"),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	docs := listDocsNamespace(t, idx, "ds")
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc-level entry, got %d (%+v)", len(docs), docs)
	}
	if docs[0].ID != "a.md" {
		t.Fatalf("doc.ID = %q, want a.md", docs[0].ID)
	}
	if want := "alpha\nbeta gamma"; docs[0].Content != want {
		t.Fatalf("doc.Content = %q, want %q", docs[0].Content, want)
	}
}

func TestRetrievalChunkRepo_Replace_RebuildsDocOnSubsequentCall(t *testing.T) {
	// A second Replace for the same (ds, docName) must overwrite the
	// doc-level Content with the new chunk concatenation, not leave
	// the previous version lingering.
	r, idx := newChunkRepoWithIndex(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "old text")}); err != nil {
		t.Fatalf("replace v1: %v", err)
	}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "new text")}); err != nil {
		t.Fatalf("replace v2: %v", err)
	}
	docs := listDocsNamespace(t, idx, "ds")
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc-level entry after rebuild, got %d", len(docs))
	}
	if docs[0].Content != "new text" {
		t.Fatalf("doc.Content = %q, want %q", docs[0].Content, "new text")
	}
}

func TestRetrievalChunkRepo_Replace_EmptyChunksClearsDocsNamespace(t *testing.T) {
	// Replace with no chunks is the canonical "doc deleted" signal
	// in the ChunkRepo contract; the doc-level namespace must
	// observe the deletion too so SearchDocs cannot resurrect it.
	r, idx := newChunkRepoWithIndex(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace seed: %v", err)
	}
	if err := r.Replace(ctx, "ds", "a.md", nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	if docs := listDocsNamespace(t, idx, "ds"); len(docs) != 0 {
		t.Fatalf("expected docs namespace empty after empty replace, got %+v", docs)
	}
}

func TestRetrievalChunkRepo_DeleteByDoc_CascadesToDocsNamespace(t *testing.T) {
	r, idx := newChunkRepoWithIndex(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace a: %v", err)
	}
	if err := r.Replace(ctx, "ds", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "beta")}); err != nil {
		t.Fatalf("replace b: %v", err)
	}
	if err := r.DeleteByDoc(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	docs := listDocsNamespace(t, idx, "ds")
	if len(docs) != 1 || docs[0].ID != "b.md" {
		t.Fatalf("expected only b.md left in docs namespace, got %+v", docs)
	}
}

func TestRetrievalChunkRepo_DeleteByDataset_CascadesToDocsNamespace(t *testing.T) {
	r, idx := newChunkRepoWithIndex(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds1", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace ds1: %v", err)
	}
	if err := r.Replace(ctx, "ds2", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "beta")}); err != nil {
		t.Fatalf("replace ds2: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds1"); err != nil {
		t.Fatalf("delete ds1: %v", err)
	}
	if docs := listDocsNamespace(t, idx, "ds1"); len(docs) != 0 {
		t.Fatalf("ds1 docs namespace not cleared: %+v", docs)
	}
	if docs := listDocsNamespace(t, idx, "ds2"); len(docs) != 1 {
		t.Fatalf("ds2 docs namespace corrupted by ds1 drop: %+v", docs)
	}
}

func TestRetrievalChunkRepo_SigRoundtrip(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	c := chunk("a.md", 0, "alpha")
	c.Sig = knowledge.DerivedSig{SourceVer: 42, ChunkerSig: "chunker:abc", EmbedSig: "openai-3"}
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{c}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(cands))
	}
	got := cands[0].Hit.Sig
	if got.SourceVer != 42 || got.ChunkerSig != "chunker:abc" || got.EmbedSig != "openai-3" {
		t.Fatalf("sig roundtrip = %+v", got)
	}
}
