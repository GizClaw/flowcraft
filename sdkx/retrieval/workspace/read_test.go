package workspace_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	wsindex "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

// docsFor builds a small canned corpus shared across read tests.
func docsFor() []retrieval.Doc {
	return []retrieval.Doc{
		{
			ID: "alpha", Content: "the quick brown fox jumps over the lazy dog",
			Vector:   []float32{1, 0, 0, 0},
			Metadata: map[string]any{"kind": "fable", "len": 9},
		},
		{
			ID: "bravo", Content: "a brown fox is quick and clever",
			Vector:   []float32{0.9, 0.1, 0, 0},
			Metadata: map[string]any{"kind": "fable", "len": 7},
		},
		{
			ID: "charlie", Content: "lorem ipsum dolor sit amet consectetur",
			Vector:   []float32{0, 1, 0, 0},
			Metadata: map[string]any{"kind": "filler", "len": 6},
		},
		{
			ID: "delta", Content: "machine learning models learn patterns from data",
			Vector:   []float32{0, 0, 1, 0},
			Metadata: map[string]any{"kind": "tech", "len": 7},
		},
	}
}

// flushedIdx builds an index, ingests the canned corpus, and
// flushes so the read path exercises segment reads (not memtable).
func flushedIdx(t *testing.T) *wsindex.Index {
	t.Helper()
	idx, _ := newIdx(t)
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns", docsFor()); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestSearch_BM25_RanksTermMatches(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: "brown fox",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 2 {
		t.Fatalf("len(hits)=%d want >=2; hits=%+v", len(resp.Hits), resp.Hits)
	}
	got := resp.Hits[0].Doc.ID
	if got != "alpha" && got != "bravo" {
		t.Errorf("top hit = %q, want one of [alpha bravo]", got)
	}
	// charlie tokenizes to no query-matching terms, so it must
	// rank below alpha/bravo even though the contract keeps
	// zero-score docs in the ranked list.
	rank := map[string]int{}
	for i, h := range resp.Hits {
		rank[h.Doc.ID] = i
	}
	if cR, ok := rank["charlie"]; ok {
		for _, top := range []string{"alpha", "bravo"} {
			if tR, ok := rank[top]; ok && cR < tR {
				t.Errorf("charlie ranked above %s: ranks=%v", top, rank)
			}
		}
	}
}

func TestSearch_Vector_PrefersClosestEmbedding(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryVector: []float32{1, 0, 0, 0},
		TopK:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) == 0 {
		t.Fatal("expected at least one vector hit")
	}
	if resp.Hits[0].Doc.ID != "alpha" {
		t.Errorf("top vector hit = %q, want alpha", resp.Hits[0].Doc.ID)
	}
}

func TestSearch_HybridUsesRRF(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText:   "brown fox",
		QueryVector: []float32{0.9, 0.1, 0, 0},
		TopK:        3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) == 0 {
		t.Fatal("expected hybrid hits")
	}
	for _, h := range resp.Hits {
		if _, ok := h.Scores["rrf"]; !ok {
			t.Errorf("hybrid hit missing rrf score: %+v", h.Scores)
		}
	}
}

func TestSearch_FilterPushdown(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: "fox",
		Filter:    retrieval.Filter{Eq: map[string]any{"kind": "fable"}},
		TopK:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range resp.Hits {
		if h.Doc.Metadata["kind"] != "fable" {
			t.Errorf("filter not applied: %s kind=%v", h.Doc.ID, h.Doc.Metadata["kind"])
		}
	}
}

func TestSearch_RespectsMinScoreSingleModality(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: "fox",
		TopK:      10,
		MinScore:  1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Errorf("MinScore=∞ should drop all hits; got %d", len(resp.Hits))
	}
}

func TestSearch_TombstoneSuppressesOlderSegment(t *testing.T) {
	idx, _ := newIdx(t)
	ctx := context.Background()
	d := docsFor()[0]
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{d}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "ns", []string{d.ID}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "fox",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range resp.Hits {
		if h.Doc.ID == d.ID {
			t.Errorf("tombstoned %q resurfaced", d.ID)
		}
	}
}

func TestSearch_MemtableOverridesSegment(t *testing.T) {
	idx, _ := newIdx(t)
	ctx := context.Background()
	docs := docsFor()
	if err := idx.Upsert(ctx, "ns", docs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	// Stage a fresh version of "alpha" with new content; if the
	// memtable wins, the search for "wolf" should surface alpha
	// and "fox" should not return alpha's stale (segment) copy.
	updated := docs[0]
	updated.Content = "the quick brown wolf strolls across the meadow"
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{updated}); err != nil {
		t.Fatal(err)
	}
	wolf, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "wolf", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(wolf.Hits) == 0 || wolf.Hits[0].Doc.ID != "alpha" {
		t.Errorf("wolf search did not surface updated alpha: %+v", wolf.Hits)
	}
	fox, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "fox", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range fox.Hits {
		if h.Doc.ID == "alpha" && !strings.Contains(h.Doc.Content, "wolf") {
			t.Errorf("alpha returned with stale segment content: %q", h.Doc.Content)
		}
	}
}

func TestFreshMemtableUpsertSurvivesOlderSegmentTombstone(t *testing.T) {
	idx, _ := newIdx(t)
	ctx := context.Background()
	old := retrieval.Doc{ID: "x", Content: "old fox", Metadata: map[string]any{"version": "old"}}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{old}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, "ns", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	fresh := retrieval.Doc{ID: "x", Content: "fresh wolf", Metadata: map[string]any{"version": "new"}}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{fresh}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := idx.Get(ctx, "ns", "x")
	if err != nil || !ok {
		t.Fatalf("Get fresh x ok=%v err=%v", ok, err)
	}
	if got.Content != "fresh wolf" {
		t.Fatalf("Get returned stale content %q", got.Content)
	}
	search, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "wolf", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) == 0 || search.Hits[0].Doc.ID != "x" || search.Hits[0].Doc.Content != "fresh wolf" {
		t.Fatalf("Search did not return fresh x: %+v", search.Hits)
	}
	list, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 10, OrderBy: retrieval.OrderByIDAsc})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != "x" || list.Items[0].Content != "fresh wolf" {
		t.Fatalf("List did not return fresh x: %+v", list.Items)
	}
	d, ok := any(idx).(retrieval.DeletableByFilter)
	if !ok {
		t.Fatal("workspace Index should implement DeletableByFilter")
	}
	n, err := d.DeleteByFilter(ctx, "ns", retrieval.Filter{Eq: map[string]any{"version": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteByFilter deleted %d, want 1", n)
	}
}

func TestSearch_NoQueryReturnsErrNoQuery(t *testing.T) {
	idx := flushedIdx(t)
	_, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{TopK: 5})
	if err == nil {
		t.Fatal("expected ErrNoQuery, got nil")
	}
}

func TestSearch_EmptyNamespaceIsEmptyResponse(t *testing.T) {
	idx, _ := newIdx(t)
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: "anything",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 0 {
		t.Errorf("len(hits)=%d on empty ns; want 0", len(resp.Hits))
	}
}

func TestList_ReturnsAllDocs(t *testing.T) {
	idx := flushedIdx(t)
	resp, err := idx.List(context.Background(), "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if int(resp.Total) != len(docsFor()) {
		t.Errorf("Total=%d want %d", resp.Total, len(docsFor()))
	}
	if len(resp.Items) != len(docsFor()) {
		t.Errorf("len(Items)=%d want %d", len(resp.Items), len(docsFor()))
	}
}

func TestList_FilterAndPagination(t *testing.T) {
	idx := flushedIdx(t)
	ctx := context.Background()
	resp1, err := idx.List(ctx, "ns", retrieval.ListRequest{
		Filter:   retrieval.Filter{Eq: map[string]any{"kind": "fable"}},
		PageSize: 1,
		OrderBy:  retrieval.OrderByIDAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp1.Total != 2 {
		t.Errorf("Total=%d want 2", resp1.Total)
	}
	if len(resp1.Items) != 1 || resp1.Items[0].ID != "alpha" {
		t.Errorf("page1 = %+v, want [alpha]", resp1.Items)
	}
	if resp1.NextPageToken == "" {
		t.Fatal("expected NextPageToken")
	}
	resp2, err := idx.List(ctx, "ns", retrieval.ListRequest{
		Filter:    retrieval.Filter{Eq: map[string]any{"kind": "fable"}},
		PageSize:  1,
		OrderBy:   retrieval.OrderByIDAsc,
		PageToken: resp1.NextPageToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Items) != 1 || resp2.Items[0].ID != "bravo" {
		t.Errorf("page2 = %+v, want [bravo]", resp2.Items)
	}
}

func TestList_DropsTombstoned(t *testing.T) {
	idx := flushedIdx(t)
	ctx := context.Background()
	if err := idx.Delete(ctx, "ns", []string{"charlie"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Items {
		if d.ID == "charlie" {
			t.Errorf("tombstoned charlie returned by List")
		}
	}
}

func TestGet_HitsMissesAndTombstones(t *testing.T) {
	idx := flushedIdx(t)
	ctx := context.Background()

	d, ok, err := idx.Get(ctx, "ns", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get(alpha) miss; want hit")
	}
	if d.ID != "alpha" {
		t.Errorf("Get returned %q", d.ID)
	}

	if _, ok, err := idx.Get(ctx, "ns", "nope"); err != nil || ok {
		t.Errorf("Get(nope) ok=%v err=%v; want ok=false err=nil", ok, err)
	}

	if err := idx.Delete(ctx, "ns", []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(ctx, "ns", "alpha"); ok {
		t.Errorf("Get(alpha) ok=true after tombstone")
	}
}

func TestDeleteByFilter_RemovesMatchingDocs(t *testing.T) {
	idx := flushedIdx(t)
	ctx := context.Background()
	d, ok := any(idx).(retrieval.DeletableByFilter)
	if !ok {
		t.Fatal("workspace Index should implement DeletableByFilter")
	}
	n, err := d.DeleteByFilter(ctx, "ns",
		retrieval.Filter{Eq: map[string]any{"kind": "fable"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("DeleteByFilter deleted %d, want 2 (alpha+bravo)", n)
	}
	resp, err := idx.List(ctx, "ns", retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range resp.Items {
		if doc.Metadata["kind"] == "fable" {
			t.Errorf("DeleteByFilter missed %s", doc.ID)
		}
	}
}

func TestDeleteByFilter_EmptyFilterRejected(t *testing.T) {
	idx := flushedIdx(t)
	d := any(idx).(retrieval.DeletableByFilter)
	_, err := d.DeleteByFilter(context.Background(), "ns", retrieval.Filter{})
	if err == nil {
		t.Fatal("expected ErrEmptyDeleteFilter")
	}
	if !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("err = %v, want ErrEmptyDeleteFilter", err)
	}
}

// TestSearch_BM25_ScoreIsSegmentationInvariant guards the
// regression in issue #125: BM25 IDF must be computed against a
// global corpus, so each doc's score must NOT depend on how the
// corpus is split across segments.
//
// We construct the same corpus twice — once flushed as a single
// segment, once forced into many small segments via a tight
// MemtableMaxDocs — and assert per-doc BM25 scores match. Under
// per-segment corpus stats the IDF differs across segments and
// this test fails by tens of percent on terms with skewed df.
func TestSearch_BM25_ScoreIsSegmentationInvariant(t *testing.T) {
	docs := bm25CorpusForRanking()

	scoreSingle := bm25Scores(t, buildAndFlushAll(t, docs, 1024 /* one segment */),
		"machine learning")
	scoreSplit := bm25Scores(t, buildAndFlushAll(t, docs, 3 /* ~7 segments */),
		"machine learning")

	if len(scoreSingle) != len(docs) {
		t.Fatalf("expected %d hits in single-segment search, got %d", len(docs), len(scoreSingle))
	}
	if len(scoreSingle) != len(scoreSplit) {
		t.Fatalf("hit count diverges: single=%d split=%d", len(scoreSingle), len(scoreSplit))
	}
	for id, single := range scoreSingle {
		split, ok := scoreSplit[id]
		if !ok {
			t.Errorf("doc %q present in single but missing from split", id)
			continue
		}
		// Floating point is exact here because both paths sum the
		// same terms in the same order against an identical
		// corpus; a tolerance would mask a real regression.
		if single != split {
			t.Errorf("BM25 score for %q diverges: single=%g split=%g", id, single, split)
		}
	}
}

// TestSearch_BM25_MemtableMatchesSegmentScore guards the second
// half of the issue #125 fix: a doc's BM25 score must not jump
// when its host writer transitions from memtable to segment, given
// the same surrounding corpus. Pre-fix this failed because
// memtable docs scored against a synthetic 1-doc corpus while
// segment docs scored against the segment-local corpus.
func TestSearch_BM25_MemtableMatchesSegmentScore(t *testing.T) {
	docs := bm25CorpusForRanking()

	preIdx, _ := newIdx(t, wsindex.WithMemtableMaxDocs(1024))
	if err := preIdx.Upsert(context.Background(), "ns", docs); err != nil {
		t.Fatal(err)
	}
	pre := bm25Scores(t, preIdx, "machine learning")

	post := bm25Scores(t, buildAndFlushAll(t, docs, 1024), "machine learning")

	if len(pre) != len(post) {
		t.Fatalf("hit count diverges memtable=%d segment=%d", len(pre), len(post))
	}
	for id, pv := range pre {
		ppv, ok := post[id]
		if !ok {
			t.Errorf("doc %q present in memtable result but missing from segment result", id)
			continue
		}
		if pv != ppv {
			t.Errorf("BM25 score for %q diverges: memtable=%g segment=%g", id, pv, ppv)
		}
	}
}

// bm25CorpusForRanking returns a small but rank-meaningful corpus.
// Every doc shares some vocab so DocFreq is non-trivial and the
// per-segment-vs-global IDF gap is observable.
func bm25CorpusForRanking() []retrieval.Doc {
	return []retrieval.Doc{
		{ID: "d01", Content: "machine learning models learn patterns from data"},
		{ID: "d02", Content: "deep learning networks recognize patterns in images"},
		{ID: "d03", Content: "machine machine learning learning is everywhere now"},
		{ID: "d04", Content: "data data data pipelines feed learning systems"},
		{ID: "d05", Content: "fox jumps over lazy dog under bright sun"},
		{ID: "d06", Content: "machine vision detects objects in real time scenes"},
		{ID: "d07", Content: "graph models capture structural patterns in networks"},
		{ID: "d08", Content: "statistical learning theory underpins many machine methods"},
		{ID: "d09", Content: "the lazy fox does not chase any dog at all"},
		{ID: "d10", Content: "patterns in data inform learning rates and choices"},
		{ID: "d11", Content: "robotics combines machine vision and machine learning"},
		{ID: "d12", Content: "lemma the proof relies on continuity not convergence"},
		{ID: "d13", Content: "music music music notes drift across the lazy river"},
		{ID: "d14", Content: "machine code differs from machine learning programs"},
		{ID: "d15", Content: "dataset shift breaks learning guarantees in practice"},
		{ID: "d16", Content: "kernel methods are a venerable learning approach"},
		{ID: "d17", Content: "feature engineering still matters for many learning tasks"},
		{ID: "d18", Content: "embedding learning surfaces latent semantic structure"},
		{ID: "d19", Content: "online learning adapts as new examples arrive"},
		{ID: "d20", Content: "machine learning at scale demands careful engineering"},
	}
}

// buildAndFlushAll ingests docs in flushPer-sized batches, flushing
// after each batch so the resulting index has roughly len(docs)/flushPer
// segments. flushPer >= len(docs) yields a single segment.
func buildAndFlushAll(t *testing.T, docs []retrieval.Doc, flushPer int) *wsindex.Index {
	t.Helper()
	idx, _ := newIdx(t, wsindex.WithMemtableMaxDocs(flushPer), wsindex.WithAutoCompact(false))
	ctx := context.Background()
	for start := 0; start < len(docs); start += flushPer {
		end := start + flushPer
		if end > len(docs) {
			end = len(docs)
		}
		if err := idx.Upsert(ctx, "ns", docs[start:end]); err != nil {
			t.Fatal(err)
		}
		if err := idx.Flush(ctx, "ns"); err != nil {
			t.Fatal(err)
		}
	}
	return idx
}

// bm25Scores runs a Search and returns the per-doc BM25 lane
// score keyed by doc ID. Compares cleanly even when ties make rank
// order nondeterministic — the score itself is the BM25 protocol
// guarantee, not the rank tiebreaker.
func bm25Scores(t *testing.T, idx *wsindex.Index, query string) map[string]float64 {
	t.Helper()
	resp, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: query,
		TopK:      1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]float64, len(resp.Hits))
	for _, h := range resp.Hits {
		out[h.Doc.ID] = h.Score
	}
	return out
}

func TestSearch_ChecksumDetectsCorruption(t *testing.T) {
	idx, ws := newIdx(t)
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns", docsFor()); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	entries, err := ws.List(ctx, "ns/segments")
	if err != nil {
		t.Fatal(err)
	}
	var segDir string
	for _, e := range entries {
		if e.IsDir() {
			segDir = "ns/segments/" + e.Name()
			break
		}
	}
	if segDir == "" {
		t.Fatal("no segment dir found")
	}
	docsPath := segDir + "/docs.jsonl"
	raw, err := ws.Read(ctx, docsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 5 {
		raw[5] ^= 0xFF
	}
	if err := ws.Write(ctx, docsPath, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "fox", TopK: 5}); err == nil {
		t.Errorf("expected corruption error, got nil")
	}
}
