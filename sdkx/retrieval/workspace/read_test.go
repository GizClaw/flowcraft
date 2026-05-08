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
