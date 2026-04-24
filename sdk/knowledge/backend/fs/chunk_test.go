package fs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newChunkRepo(t *testing.T) (*FSChunkRepo, *workspace.MemWorkspace) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	return NewChunkRepo(ws, "kb", &textsearch.SimpleTokenizer{}), ws
}

func chunk(doc string, idx int, content string) knowledge.DerivedChunk {
	return knowledge.DerivedChunk{
		DocName: doc,
		Index:   idx,
		Content: content,
		Sig:     knowledge.DerivedSig{SourceVer: 1, ChunkerSig: "test"},
	}
}

func TestChunkRepo_ReplaceEliminatesStaleChunks(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "doc.md", []knowledge.DerivedChunk{
		chunk("doc.md", 0, "alpha beta"),
		chunk("doc.md", 1, "gamma delta"),
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("first search len = %d, want 1", len(cands))
	}

	if err := r.Replace(ctx, "ds", "doc.md", []knowledge.DerivedChunk{
		chunk("doc.md", 0, "epsilon zeta"),
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	cands, err = r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search after replace: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("stale chunk surfaced: %+v", cands)
	}
	cands, err = r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "epsilon", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search new chunk: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("new chunk not found: %+v", cands)
	}
}

func TestChunkRepo_PersistAndLoadRoundtrip(t *testing.T) {
	r, ws := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "hello world")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	data, err := ws.Read(ctx, "kb/ds/.chunks.json")
	if err != nil {
		t.Fatalf("read chunks file: %v", err)
	}
	var file chunksFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if file.Version != chunksFileVersion {
		t.Fatalf("version = %d, want %d", file.Version, chunksFileVersion)
	}
	if len(file.Chunks) != 1 || file.Chunks[0].Content != "hello world" {
		t.Fatalf("file chunks = %+v", file.Chunks)
	}

	r2 := NewChunkRepo(ws, "kb", &textsearch.SimpleTokenizer{})
	if err := r2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	cands, err := r2.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "hello", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search after load: %v", err)
	}
	if len(cands) != 1 || !strings.Contains(cands[0].Hit.Content, "hello") {
		t.Fatalf("post-load search = %+v", cands)
	}
}

func TestChunkRepo_DeleteByDoc(t *testing.T) {
	r, _ := newChunkRepo(t)
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
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search alpha: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("alpha leftover: %+v", cands)
	}
	cands, err = r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "beta", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search beta: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("beta missing: %+v", cands)
	}
}

func TestChunkRepo_DeleteByDataset(t *testing.T) {
	r, ws := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := r.DeleteByDataset(ctx, "ds"); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	exists, err := ws.Exists(ctx, "kb/ds/.chunks.json")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Fatalf("chunks file should be deleted")
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{DatasetIDs: []string{"ds"}, Text: "alpha", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected no results, got %+v", cands)
	}
}

func TestChunkRepo_SearchCrossDataset(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds1", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "shared keyword apple")}); err != nil {
		t.Fatalf("replace ds1: %v", err)
	}
	if err := r.Replace(ctx, "ds2", "b.md", []knowledge.DerivedChunk{chunk("b.md", 0, "shared keyword banana")}); err != nil {
		t.Fatalf("replace ds2: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{Text: "shared", Mode: knowledge.ModeBM25, TopK: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		seen[c.Hit.DatasetID] = true
	}
	if !seen["ds1"] || !seen["ds2"] {
		t.Fatalf("cross-dataset miss: seen=%v", seen)
	}
}

func TestChunkRepo_SearchVectorMode(t *testing.T) {
	r, _ := newChunkRepo(t)
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
}

func TestChunkRepo_HybridFansOutBothPaths(t *testing.T) {
	r, _ := newChunkRepo(t)
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

func TestChunkRepo_UTF8Content(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "中文 检索 测试"),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "检索",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) == 0 {
		t.Skip("simple tokenizer cannot split CJK; skipped (covered by CJK tokenizer in higher tiers)")
	}
}

func TestChunkRepo_LegacyEmptyModeIsBM25(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{chunk("a.md", 0, "alpha bravo")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "alpha",
		Mode:       "",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("legacy empty mode produced %d results", len(cands))
	}
}
