package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
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

func TestRetrievalChunkRepo_SearchDocs_RanksDocAcrossSplitTerms(t *testing.T) {
	// A doc whose query keywords are SPLIT across two chunks must
	// outrank a noise doc that only matches one of those keywords
	// (even repeatedly) in a single chunk. This is the exact
	// failure mode of max-pool chunk-collapse documented in #126.
	// Under the doc-level namespace, the concatenated Content sees
	// both keywords as one doc and BM25 scores it accordingly,
	// without the sum-pool double-counting hazard #137 introduced.
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
		t.Fatalf("top doc = %q, want rel.md (split-term coverage should beat single-term repetition at doc-level); ranking = %+v",
			cands[0].Hit.DocName, cands)
	}
}

func TestRetrievalChunkRepo_SearchDocs_ModeVectorReturnsNotAvailable(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	_, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeVector,
		TopK:       5,
	})
	if err == nil {
		t.Fatalf("expected NotAvailable error for ModeVector, got nil")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable, got %v", err)
	}
}

func TestRetrievalChunkRepo_SearchDocs_ModeHybridReturnsNotAvailable(t *testing.T) {
	r := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	_, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "alpha",
		Vector:     []float32{1, 0, 0},
		Mode:       knowledge.ModeHybrid,
		TopK:       5,
	})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable for ModeHybrid, got %v", err)
	}
}

func TestRetrievalChunkRepo_SearchDocs_CorpusStatsAreDocLevel(t *testing.T) {
	// Seed 100 docs where exactly ONE doc contains the term "rare"
	// and EVERY doc contains the term "common". A doc-level BM25
	// scorer working on doc-level N / df / avgdl will surface the
	// rare-bearing doc at the top of a "rare" query and rank every
	// other doc above zero only for "common" — but the rare doc's
	// score for "rare" must be much higher than the common-only
	// docs' score for "common" because rare has df=1 while common
	// has df=100. This is the exact regime the chunk-level +
	// sum-pool implementation cannot reproduce (#134 root cause:
	// chunk-level N inflates and IDF collapses toward log(2)).
	r := newChunkRepo(t)
	ctx := context.Background()
	const N = 100
	for i := 0; i < N; i++ {
		name := "doc" + itoa(i) + ".md"
		content := "common token"
		if i == 0 {
			content = "rare common token"
		}
		if err := r.Replace(ctx, "ds", name, []knowledge.DerivedChunk{chunk(name, 0, content)}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	rare, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"}, Text: "rare", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if err != nil {
		t.Fatalf("rare search: %v", err)
	}
	if len(rare) != 1 || rare[0].Hit.DocName != "doc0.md" {
		t.Fatalf("rare query: expected exactly doc0.md, got %+v", rare)
	}
	common, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"}, Text: "common", Mode: knowledge.ModeBM25, TopK: 5,
	})
	if err != nil {
		t.Fatalf("common search: %v", err)
	}
	if len(common) == 0 {
		t.Fatalf("common query returned nothing")
	}
	// The rare doc's score for "rare" (df=1, N=100) must be
	// strictly greater than any common doc's score for "common"
	// (df=100, N=100) — IDF saturates to ~0 when df==N. This is
	// the smoking gun that the scorer is working on doc-level
	// stats, not chunk-level.
	if rare[0].Hit.Score <= common[0].Hit.Score {
		t.Fatalf("doc-level IDF discrimination failed: rare.score=%v common.score=%v "+
			"(rare term df=1/N=100 should dominate common term df=100/N=100)",
			rare[0].Hit.Score, common[0].Hit.Score)
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

func TestRetrievalChunkRepo_SearchDocs_BM25OnlySourceLabel(t *testing.T) {
	// SearchDocs is BM25-only on the v1 path (vector/hybrid return
	// NotAvailable). Every Candidate must carry Source="bm25" so
	// downstream lane-aware code does not have to special-case
	// doc-level output.
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
		t.Fatalf("doc-level Source = %q, want \"bm25\"", got)
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
