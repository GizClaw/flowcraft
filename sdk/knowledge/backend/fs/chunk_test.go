package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

// TestChunkRepo_Replace_ConcurrentDoesNotLoseDocs exercises the
// previously-racy Replace path that lost docs under concurrent ingest.
// Each goroutine writes a distinct (docName, content) pair where the
// content contains both a unique token and a shared one; a final BM25
// search for the shared token must return all N docs. Pre-fix, the
// observed loss rate on BEIR scifact at ingest_concurrency=8 was ~60%
// of docs (nDCG@10 dropping from 0.180 to 0.071); this test is small
// enough to keep CI under a second yet large enough that the race
// reliably manifested before the fix.
func TestChunkRepo_Replace_ConcurrentDoesNotLoseDocs(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	const N = 200

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("doc%d.md", i)
			content := fmt.Sprintf("uniquetoken%d sharedtoken", i)
			c := chunk(name, 0, content)
			if err := r.Replace(ctx, "ds", name, []knowledge.DerivedChunk{c}); err != nil {
				t.Errorf("replace %s: %v", name, err)
			}
		}()
	}
	wg.Wait()

	cands, err := r.Search(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "sharedtoken",
		Mode:       knowledge.ModeBM25,
		TopK:       N * 4,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := make(map[string]bool, N)
	for _, c := range cands {
		seen[c.Hit.DocName] = true
	}
	if len(seen) != N {
		t.Fatalf("doc-loss race regressed: %d/%d docs survived concurrent ingest", len(seen), N)
	}
}

// TestChunkRepo_Replace_ConcurrentDocLevelStateIsConsistent verifies
// that the doc-level state (added in #127) is also rebuilt
// consistently when Replace runs concurrently. The doc-level index is
// rebuilt by buildState off the same merged slice as the chunk-level
// index, so a race that loses docs from one would also lose them from
// the other; this test checks SearchDocs explicitly to lock in that
// the two indices stay in lock-step under concurrency.
func TestChunkRepo_Replace_ConcurrentDocLevelStateIsConsistent(t *testing.T) {
	r, _ := newChunkRepo(t)
	ctx := context.Background()
	const N = 100

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("doc%d.md", i)
			content := fmt.Sprintf("uniquetoken%d sharedtoken", i)
			if err := r.Replace(ctx, "ds", name, []knowledge.DerivedChunk{chunk(name, 0, content)}); err != nil {
				t.Errorf("replace %s: %v", name, err)
			}
		}()
	}
	wg.Wait()

	cands, err := r.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "sharedtoken",
		Mode:       knowledge.ModeBM25,
		TopK:       N * 4,
	})
	if err != nil {
		t.Fatalf("search docs: %v", err)
	}
	seen := make(map[string]bool, N)
	for _, c := range cands {
		seen[c.Hit.DocName] = true
	}
	if len(seen) != N {
		t.Fatalf("doc-level state out of sync: %d/%d docs surfaced at doc-level after concurrent ingest", len(seen), N)
	}
}

func TestChunkRepo_SearchDocs_ImplementsDocLevelSearcher(t *testing.T) {
	r, _ := newChunkRepo(t)
	if _, ok := any(r).(knowledge.DocLevelSearcher); !ok {
		t.Fatalf("FSChunkRepo must implement knowledge.DocLevelSearcher")
	}
}

func TestChunkRepo_SearchDocs_AggregatesAcrossChunks(t *testing.T) {
	// A doc whose query keywords are SPLIT across two chunks must
	// outrank a non-relevant doc that dense-hits one of those
	// keywords in a single chunk. This is the exact failure mode of
	// max-pool chunk-collapse documented in #126; doc-level scoring
	// folds the per-chunk evidence into one doc score.
	r, _ := newChunkRepo(t)
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
	// Exactly one Hit per doc; ChunkIndex must be -1; Layer empty.
	seen := map[string]int{}
	for _, c := range cands {
		seen[c.Hit.DocName]++
		if c.Hit.ChunkIndex != -1 {
			t.Fatalf("doc-level hit has ChunkIndex %d, want -1", c.Hit.ChunkIndex)
		}
		if c.Hit.Layer != "" {
			t.Fatalf("doc-level hit has Layer %q, want \"\"", c.Hit.Layer)
		}
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("docName %q surfaced %d times, want 1", name, n)
		}
	}
	// rel.md should outrank noise.md: it covers BOTH keywords (one
	// per chunk → tf=1 each at doc level for both terms), whereas
	// noise.md only covers foxtrot once and alpha 3x — but alpha has
	// half the IDF since both docs contain it.
	if cands[0].Hit.DocName != "rel.md" {
		t.Fatalf("top doc = %q, want rel.md; full ranking = %+v",
			cands[0].Hit.DocName, cands)
	}
}

func TestChunkRepo_SearchDocs_NoDoubleCountAcrossChunksOfSameDoc(t *testing.T) {
	// A query whose keyword appears in N chunks of the same doc must
	// still produce exactly ONE doc-level hit for that doc.
	r, _ := newChunkRepo(t)
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
}

func TestChunkRepo_SearchDocs_RebuildsOnReplace(t *testing.T) {
	// Doc-level state must be rebuilt by Replace just like the
	// chunk-level index; stale docs must not surface after their
	// last chunk is removed.
	r, _ := newChunkRepo(t)
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

func TestChunkRepo_SearchDocs_SurvivesPersistLoadRoundtrip(t *testing.T) {
	// Doc-level index is derived state; a freshly-loaded repo must
	// rebuild it correctly from the persisted chunks file.
	r, ws := newChunkRepo(t)
	ctx := context.Background()
	if err := r.Replace(ctx, "ds", "a.md", []knowledge.DerivedChunk{
		chunk("a.md", 0, "alpha bravo"),
		chunk("a.md", 1, "charlie delta"),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	r2 := NewChunkRepo(ws, "kb", &textsearch.SimpleTokenizer{})
	if err := r2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	cands, err := r2.SearchDocs(ctx, knowledge.ChunkQuery{
		DatasetIDs: []string{"ds"},
		Text:       "alpha charlie",
		Mode:       knowledge.ModeBM25,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("post-load search: %v", err)
	}
	if len(cands) != 1 || cands[0].Hit.DocName != "a.md" {
		t.Fatalf("post-load doc-level search = %+v", cands)
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
