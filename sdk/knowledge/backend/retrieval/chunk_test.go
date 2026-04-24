package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func newChunkRepo(t *testing.T) *RetrievalChunkRepo {
	t.Helper()
	return NewChunkRepo(memory.New())
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
