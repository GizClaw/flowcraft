package contract

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Factory builds an empty Index plus a cleanup func.
type Factory func(t *testing.T) (retrieval.Index, func())

// Run executes all generic contract tests on the Factory.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("UpsertGetDelete", func(t *testing.T) { testUpsertGetDelete(t, f) })
	t.Run("UpsertIdempotent", func(t *testing.T) { testUpsertIdempotent(t, f) })
	t.Run("ReadAfterWrite", func(t *testing.T) { testReadAfterWrite(t, f) })
	t.Run("NamespaceIsolation", func(t *testing.T) { testNamespaceIsolation(t, f) })
	t.Run("SearchNoQuery", func(t *testing.T) { testSearchNoQuery(t, f) })
	t.Run("SearchWhitespaceQueryIsNoQuery", func(t *testing.T) { testSearchWhitespaceQueryIsNoQuery(t, f) })
	t.Run("WhitespaceQueryVectorIsVectorOnly", func(t *testing.T) { testWhitespaceQueryVectorIsVectorOnly(t, f) })
	t.Run("ListPagination", func(t *testing.T) { testListPagination(t, f) })
	t.Run("ListPaginationStalePageToken", func(t *testing.T) { testListPaginationStalePageToken(t, f) })
	t.Run("FilterEqAndIn", func(t *testing.T) { testFilterEqIn(t, f) })
	t.Run("FilterRangeAndExists", func(t *testing.T) { testFilterRangeExists(t, f) })
	t.Run("FilterNotComposes", func(t *testing.T) { testFilterNotComposes(t, f) })
	t.Run("BM25CapabilityMatchesSearch", func(t *testing.T) { testBM25CapabilityMatchesSearch(t, f) })
	t.Run("VectorCapabilityMatchesSearch", func(t *testing.T) { testVectorCapabilityMatchesSearch(t, f) })
	t.Run("SparseCapabilityMatchesSearch", func(t *testing.T) { testSparseCapabilityMatchesSearch(t, f) })
	t.Run("HybridCapabilityMatchesSearchModes", func(t *testing.T) { testHybridCapabilityMatchesSearchModes(t, f) })
	t.Run("HybridInvalidModeAndParams", func(t *testing.T) { testHybridInvalidModeAndParams(t, f) })
	t.Run("SparseHybridCapabilityMatchesSearch", func(t *testing.T) { testSparseHybridCapabilityMatchesSearch(t, f) })
	t.Run("HybridIgnoresSearchMinScore", func(t *testing.T) { testHybridIgnoresSearchMinScore(t, f) })
	t.Run("HybridDropsZeroEvidenceDocs", func(t *testing.T) { testHybridDropsZeroEvidenceDocs(t, f) })
	t.Run("ListWithVectorFalseDropsAllVectors", func(t *testing.T) { testListWithVectorFalseDropsAllVectors(t, f) })
	t.Run("DeleteByFilterValidation", func(t *testing.T) { testDeleteByFilterValidation(t, f) })
	t.Run("OptionalIterable", func(t *testing.T) { testOptionalIterable(t, f) })
	t.Run("OptionalDroppable", func(t *testing.T) { testOptionalDroppable(t, f) })
	t.Run("CapabilitiesShape", func(t *testing.T) { testCapabilitiesShape(t, f) })
}

func mustUpsert(t *testing.T, idx retrieval.Index, ns string, docs []retrieval.Doc) {
	t.Helper()
	if err := idx.Upsert(context.Background(), ns, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func testUpsertGetDelete(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_a"
	d := retrieval.Doc{ID: "k1", Content: "hello world", Timestamp: time.Now()}
	mustUpsert(t, idx, ns, []retrieval.Doc{d})
	g, ok := idx.(retrieval.DocGetter)
	if !ok {
		t.Skip("index does not implement DocGetter")
	}
	got, found, err := g.Get(ctx, ns, "k1")
	if err != nil || !found || got.ID != "k1" {
		t.Fatalf("get: found=%v err=%v got=%+v", found, err, got)
	}
	if err := idx.Delete(ctx, ns, []string{"k1"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := g.Get(ctx, ns, "k1"); ok {
		t.Fatal("expected miss after delete")
	}
}

func testUpsertIdempotent(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ns := "ns_idem"
	d := retrieval.Doc{ID: "x", Content: "v1", Timestamp: time.Now()}
	mustUpsert(t, idx, ns, []retrieval.Doc{d})
	mustUpsert(t, idx, ns, []retrieval.Doc{d})
	g, ok := idx.(retrieval.DocGetter)
	if !ok {
		return
	}
	got, found, _ := g.Get(context.Background(), ns, "x")
	if !found || got.Content != "v1" {
		t.Fatalf("expected idempotent v1, got %+v", got)
	}
	d2 := d
	d2.Content = "v2"
	mustUpsert(t, idx, ns, []retrieval.Doc{d2})
	got, _, _ = g.Get(context.Background(), ns, "x")
	if got.Content != "v2" {
		t.Fatalf("expected v2 after second upsert, got %+v", got)
	}
}

func testReadAfterWrite(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_raw"
	mustUpsert(t, idx, ns, []retrieval.Doc{{ID: "a", Content: "alpha bravo", Timestamp: time.Now()}})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{QueryText: "alpha", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("read-after-write failed: %+v", resp.Hits)
	}
}

func testNamespaceIsolation(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	mustUpsert(t, idx, "ns_x", []retrieval.Doc{{ID: "1", Content: "alpha", Timestamp: time.Now()}})
	mustUpsert(t, idx, "ns_y", []retrieval.Doc{{ID: "1", Content: "beta", Timestamp: time.Now()}})
	rx, _ := idx.Search(ctx, "ns_x", retrieval.SearchRequest{QueryText: "alpha", TopK: 5})
	ry, _ := idx.Search(ctx, "ns_y", retrieval.SearchRequest{QueryText: "alpha", TopK: 5})
	if len(rx.Hits) != 1 {
		t.Fatalf("ns_x expected 1 hit, got %+v", rx.Hits)
	}
	for _, h := range ry.Hits {
		if h.Doc.Content == "alpha" {
			t.Fatalf("ns_y leaked across namespace: %+v", h.Doc)
		}
	}
}

func testSearchNoQuery(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	_, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{TopK: 5})
	if !errors.Is(err, retrieval.ErrNoQuery) && !errdefs.IsValidation(err) {
		t.Fatalf("expected ErrNoQuery, got %v", err)
	}
}

func testSearchWhitespaceQueryIsNoQuery(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	_, err := idx.Search(context.Background(), "ns", retrieval.SearchRequest{
		QueryText: " \t\n ",
		TopK:      5,
	})
	if !errors.Is(err, retrieval.ErrNoQuery) && !errdefs.IsValidation(err) {
		t.Fatalf("expected whitespace-only query to be no-query, got %v", err)
	}
}

func testWhitespaceQueryVectorIsVectorOnly(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	if !retrieval.CapabilitiesOf(idx).Vector {
		t.Skip("Vector=false")
	}
	ctx := context.Background()
	ns := "ns_whitespace_vector"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "x", Content: "alpha", Vector: []float32{1, 0}, Timestamp: time.Now()},
		{ID: "y", Content: "beta", Vector: []float32{0, 1}, Timestamp: time.Now().Add(time.Second)},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   " \t\n ",
		QueryVector: []float32{0, 1},
		TopK:        2,
		HybridMode:  retrieval.HybridMode("invalid-if-hybrid"),
		HybridOptions: retrieval.HybridOptions{
			Weights: map[retrieval.SearchSignal]float64{
				retrieval.SearchSignalBM25:   0,
				retrieval.SearchSignalVector: 0,
			},
		},
	})
	if err != nil {
		t.Fatalf("whitespace QueryText must not turn vector search into hybrid: %v", err)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].Doc.ID != "y" || resp.Hits[0].Scores["cos"] <= 0 {
		t.Fatalf("whitespace+vector did not behave like vector-only search: %+v", resp.Hits)
	}
	for _, h := range resp.Hits {
		if _, ok := h.Scores["rrf"]; ok {
			t.Fatalf("whitespace+vector unexpectedly used hybrid fusion: %+v", resp.Hits)
		}
		if _, ok := h.Scores["weighted"]; ok {
			t.Fatalf("whitespace+vector unexpectedly used hybrid fusion: %+v", resp.Hits)
		}
		if _, ok := h.Scores["convex"]; ok {
			t.Fatalf("whitespace+vector unexpectedly used hybrid fusion: %+v", resp.Hits)
		}
	}
}

func testListPagination(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_list"
	base := time.Unix(1700, 0).UTC()
	docs := make([]retrieval.Doc, 0, 5)
	for i := range 5 {
		docs = append(docs, retrieval.Doc{
			ID:        string(rune('a' + i)),
			Content:   "x",
			Timestamp: base.Add(time.Duration(i) * time.Hour),
		})
	}
	mustUpsert(t, idx, ns, docs)
	r1, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Items) != 2 || r1.NextPageToken == "" {
		t.Fatalf("page1=%+v", r1)
	}
	r2, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 2, PageToken: r1.NextPageToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Items) != 2 {
		t.Fatalf("page2 len=%d", len(r2.Items))
	}
}

func testListPaginationStalePageToken(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_list_stale"
	now := time.Now()
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "a", Content: "x", Timestamp: now},
		{ID: "b", Content: "x", Timestamp: now.Add(time.Second)},
	})
	tok, err := retrieval.EncodeListPageToken(10)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 2, PageToken: tok})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected empty page for stale token, got %d items", len(resp.Items))
	}
	if resp.NextPageToken != "" {
		t.Fatalf("expected no next token, got %q", resp.NextPageToken)
	}
}

func testFilterEqIn(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_filt"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "1", Content: "alpha", Metadata: map[string]any{"cat": "x"}, Timestamp: time.Now()},
		{ID: "2", Content: "alpha", Metadata: map[string]any{"cat": "y"}, Timestamp: time.Now()},
		{ID: "3", Content: "alpha", Metadata: map[string]any{"cat": "z"}, Timestamp: time.Now()},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "alpha", TopK: 10,
		Filter: retrieval.Filter{Eq: map[string]any{"cat": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "1" {
		t.Fatalf("Eq filter wrong: %+v", resp.Hits)
	}
	resp2, _ := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "alpha", TopK: 10,
		Filter: retrieval.Filter{In: map[string][]any{"cat": {"x", "y"}}},
	})
	if len(resp2.Hits) != 2 {
		t.Fatalf("In filter wrong: %+v", resp2.Hits)
	}
}

func testFilterRangeExists(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_rng"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "1", Content: "x", Metadata: map[string]any{"score": 1.0}, Timestamp: time.Now()},
		{ID: "2", Content: "x", Metadata: map[string]any{"score": 5.0}, Timestamp: time.Now()},
		{ID: "3", Content: "x", Metadata: map[string]any{"other": true}, Timestamp: time.Now()},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "x", TopK: 10,
		Filter: retrieval.Filter{Range: map[string]retrieval.Range{"score": {Gte: 2.0}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "2" {
		t.Fatalf("Range filter wrong: %+v", resp.Hits)
	}
	resp2, _ := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "x", TopK: 10,
		Filter: retrieval.Filter{Exists: []string{"score"}},
	})
	ids := idsOf(resp2.Hits)
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("Exists filter wrong: %+v", ids)
	}
}

func testFilterNotComposes(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_not"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "1", Content: "alpha", Metadata: map[string]any{"tenant": "acme", "status": "active"}, Timestamp: time.Now()},
		{ID: "2", Content: "alpha", Metadata: map[string]any{"tenant": "other", "status": "active"}, Timestamp: time.Now()},
		{ID: "3", Content: "alpha", Metadata: map[string]any{"tenant": "acme", "status": "deleted"}, Timestamp: time.Now()},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "alpha",
		TopK:      10,
		Filter: retrieval.Filter{
			Not: &retrieval.Filter{Eq: map[string]any{"status": "deleted"}},
			Eq:  map[string]any{"tenant": "acme"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "1" {
		t.Fatalf("Not composition wrong: %+v", resp.Hits)
	}
}

func testHybridIgnoresSearchMinScore(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	if !retrieval.CapabilitiesOf(idx).Hybrid {
		t.Skip("Hybrid=false")
	}
	ctx := context.Background()
	ns := "ns_hybrid_minscore"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "a", Content: "alpha", Vector: []float32{1, 0}, Timestamp: time.Now()},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        1,
		MinScore:    1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("hybrid search must ignore SearchRequest.MinScore, hits=%+v", resp.Hits)
	}
}

func testHybridDropsZeroEvidenceDocs(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	if !retrieval.CapabilitiesOf(idx).Hybrid {
		t.Skip("Hybrid=false")
	}
	ctx := context.Background()
	ns := "ns_hybrid_zero"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "text-only", Content: "alpha", Vector: []float32{0, 1}, Timestamp: time.Now()},
		{ID: "vector-only", Content: "zzz", Vector: []float32{1, 0}, Timestamp: time.Now().Add(time.Second)},
		{ID: "zero-evidence", Content: "zzz", Vector: []float32{0, 1}, Timestamp: time.Now().Add(2 * time.Second)},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        3,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(resp.Hits)
	if !containsID(ids, "text-only") {
		t.Fatalf("hybrid search dropped text-lane positive doc: hits=%+v", resp.Hits)
	}
	if idx.Capabilities().Hybrid && !containsID(ids, "vector-only") {
		t.Fatalf("native hybrid search dropped vector-lane positive doc: hits=%+v", resp.Hits)
	}
	for _, h := range resp.Hits {
		if h.Doc.ID == "zero-evidence" {
			t.Fatalf("hybrid search returned zero-evidence doc: hits=%+v", resp.Hits)
		}
	}
}

func testBM25CapabilityMatchesSearch(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.BM25 {
		t.Skip("BM25=false")
	}
	ctx := context.Background()
	ns := "ns_bm25_cap"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "alpha", Content: "alpha alpha", Timestamp: time.Now()},
		{ID: "beta", Content: "beta beta", Timestamp: time.Now().Add(time.Second)},
	})
	alpha, err := idx.Search(ctx, ns, retrieval.SearchRequest{QueryText: "alpha", TopK: 2})
	if err != nil {
		t.Fatalf("BM25=true backend rejected text search: %v", err)
	}
	if len(alpha.Hits) == 0 || alpha.Hits[0].Doc.ID != "alpha" || alpha.Hits[0].Scores["bm25"] <= 0 {
		t.Fatalf("QueryText did not rank alpha first: %+v", alpha.Hits)
	}
	beta, err := idx.Search(ctx, ns, retrieval.SearchRequest{QueryText: "beta", TopK: 2})
	if err != nil {
		t.Fatalf("BM25=true backend rejected text search: %v", err)
	}
	if len(beta.Hits) == 0 || beta.Hits[0].Doc.ID != "beta" || beta.Hits[0].Scores["bm25"] <= 0 {
		t.Fatalf("QueryText did not rank beta first: %+v", beta.Hits)
	}
}

func testVectorCapabilityMatchesSearch(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_vector_cap"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "x", Content: "same", Vector: []float32{1, 0}, Timestamp: time.Now()},
		{ID: "y", Content: "same", Vector: []float32{0, 1}, Timestamp: time.Now().Add(time.Second)},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryVector: []float32{0, 1},
		TopK:        2,
	})
	if retrieval.CapabilitiesOf(idx).Vector {
		if err != nil {
			t.Fatalf("Vector=true backend rejected vector search: %v", err)
		}
		if len(resp.Hits) == 0 || resp.Hits[0].Doc.ID != "y" || resp.Hits[0].Scores["cos"] <= 0 {
			t.Fatalf("QueryVector did not rank y first: %+v", resp.Hits)
		}
		return
	}
	if err == nil {
		t.Fatalf("Vector=false backend must reject vector-only search, got response %+v", resp)
	}
	if !errdefs.IsValidation(err) && !errdefs.IsNotAvailable(err) && !errors.Is(err, retrieval.ErrNoQuery) {
		t.Fatalf("Vector=false backend returned unexpected error: %v", err)
	}
}

func testSparseCapabilityMatchesSearch(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_sparse"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "sparse", Content: "x", SparseVector: map[string]float32{"needle": 2}, Timestamp: time.Now()},
		{ID: "other", Content: "x", SparseVector: map[string]float32{"other": 2}, Timestamp: time.Now().Add(time.Second)},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		SparseVec: map[string]float32{"needle": 0.5},
		TopK:      2,
	})
	if retrieval.CapabilitiesOf(idx).Sparse {
		if err != nil {
			t.Fatalf("Sparse=true backend rejected sparse search: %v", err)
		}
		if len(resp.Hits) == 0 || resp.Hits[0].Doc.ID != "sparse" || resp.Hits[0].Scores["sparse"] <= 0 {
			t.Fatalf("sparse search returned wrong hits: %+v", resp.Hits)
		}
		return
	}
	if err == nil {
		t.Fatalf("Sparse=false backend must reject sparse-only search, got response %+v", resp)
	}
	if !errdefs.IsValidation(err) && !errdefs.IsNotAvailable(err) && !errors.Is(err, retrieval.ErrNoQuery) {
		t.Fatalf("Sparse=false backend returned unexpected error: %v", err)
	}
}

func testHybridCapabilityMatchesSearchModes(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Hybrid {
		t.Skip("Hybrid=false")
	}
	if !caps.BM25 || !caps.Vector {
		t.Skip("contract hybrid mode test currently requires bm25+vector")
	}
	ctx := context.Background()
	ns := "ns_hybrid_modes"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "text", Content: "alpha alpha alpha", Vector: []float32{0, 1}, Timestamp: time.Now()},
		{ID: "vector", Content: "beta beta beta", Vector: []float32{1, 0}, Timestamp: time.Now().Add(time.Second)},
	})
	alphaText := 1.0
	alphaVector := 0.0
	rrf, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        2,
		HybridMode:  retrieval.HybridRRF,
	})
	if err != nil {
		t.Fatalf("Hybrid=true backend rejected RRF: %v", err)
	}
	ids := idsOf(rrf.Hits)
	if !containsID(ids, "text") || !containsID(ids, "vector") {
		t.Fatalf("hybrid RRF ignored one signal: %+v", rrf.Hits)
	}
	textRawWeighted, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        2,
		HybridMode:  retrieval.HybridWeighted,
		HybridOptions: retrieval.HybridOptions{
			Weights: map[retrieval.SearchSignal]float64{
				retrieval.SearchSignalBM25:   1.0,
				retrieval.SearchSignalVector: 0.0,
			},
		},
	})
	if err != nil {
		t.Fatalf("Hybrid=true backend rejected weighted bm25-only weights: %v", err)
	}
	if len(textRawWeighted.Hits) != 1 || textRawWeighted.Hits[0].Doc.ID != "text" || textRawWeighted.Hits[0].Scores["weighted"] <= 0 {
		t.Fatalf("weighted bm25-only should return only text lane hit: %+v", textRawWeighted.Hits)
	}
	vectorRawWeighted, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        2,
		HybridMode:  retrieval.HybridWeighted,
		HybridOptions: retrieval.HybridOptions{
			Weights: map[retrieval.SearchSignal]float64{
				retrieval.SearchSignalBM25:   0.0,
				retrieval.SearchSignalVector: 1.0,
			},
		},
	})
	if err != nil {
		t.Fatalf("Hybrid=true backend rejected weighted vector-only weights: %v", err)
	}
	if len(vectorRawWeighted.Hits) != 1 || vectorRawWeighted.Hits[0].Doc.ID != "vector" || vectorRawWeighted.Hits[0].Scores["weighted"] <= 0 {
		t.Fatalf("zero-weight text lane leaked into weighted hits: %+v", vectorRawWeighted.Hits)
	}
	textWeighted, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:     "alpha",
		QueryVector:   []float32{1, 0},
		TopK:          2,
		HybridMode:    retrieval.HybridConvex,
		HybridOptions: retrieval.HybridOptions{Alpha: &alphaText},
	})
	if err != nil {
		t.Fatalf("Hybrid=true backend rejected convex alpha=1: %v", err)
	}
	if len(textWeighted.Hits) == 0 || textWeighted.Hits[0].Doc.ID != "text" || textWeighted.Hits[0].Scores["convex"] <= 0 {
		t.Fatalf("convex alpha=1 should prefer text lane: %+v", textWeighted.Hits)
	}
	vectorWeighted, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:     "alpha",
		QueryVector:   []float32{1, 0},
		TopK:          2,
		HybridMode:    retrieval.HybridConvex,
		HybridOptions: retrieval.HybridOptions{Alpha: &alphaVector},
	})
	if err != nil {
		t.Fatalf("Hybrid=true backend rejected convex alpha=0: %v", err)
	}
	if len(vectorWeighted.Hits) == 0 || vectorWeighted.Hits[0].Doc.ID != "vector" || vectorWeighted.Hits[0].Scores["convex"] <= 0 {
		t.Fatalf("convex alpha=0 should prefer vector lane: %+v", vectorWeighted.Hits)
	}
}

func testHybridInvalidModeAndParams(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Hybrid {
		t.Skip("Hybrid=false")
	}
	if !caps.BM25 || !caps.Vector {
		t.Skip("contract hybrid validation test currently requires bm25+vector")
	}
	ctx := context.Background()
	ns := "ns_hybrid_invalid"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "text", Content: "alpha", Vector: []float32{0, 1}, Timestamp: time.Now()},
		{ID: "vector", Content: "beta", Vector: []float32{1, 0}, Timestamp: time.Now().Add(time.Second)},
	})
	alpha := 0.5
	cases := []struct {
		name string
		req  retrieval.SearchRequest
	}{
		{
			name: "invalid mode",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridMode("bogus"),
			},
		},
		{
			name: "negative rrf k",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridRRF,
				HybridOptions: retrieval.HybridOptions{
					K: -1,
				},
			},
		},
		{
			name: "all zero weighted weights",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridWeighted,
				HybridOptions: retrieval.HybridOptions{
					Weights: map[retrieval.SearchSignal]float64{
						retrieval.SearchSignalBM25:   0,
						retrieval.SearchSignalVector: 0,
					},
				},
			},
		},
		{
			name: "all zero convex weights",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridConvex,
				HybridOptions: retrieval.HybridOptions{
					Weights: map[retrieval.SearchSignal]float64{
						retrieval.SearchSignalBM25:   0,
						retrieval.SearchSignalVector: 0,
					},
				},
			},
		},
		{
			name: "rrf rejects weights",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridRRF,
				HybridOptions: retrieval.HybridOptions{
					Weights: map[retrieval.SearchSignal]float64{
						retrieval.SearchSignalBM25: 1,
					},
				},
			},
		},
		{
			name: "rrf rejects alpha",
			req: retrieval.SearchRequest{
				QueryText:     "alpha",
				QueryVector:   []float32{1, 0},
				TopK:          2,
				HybridMode:    retrieval.HybridRRF,
				HybridOptions: retrieval.HybridOptions{Alpha: &alpha},
			},
		},
		{
			name: "weighted rejects k",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridWeighted,
				HybridOptions: retrieval.HybridOptions{
					K: 60,
				},
			},
		},
		{
			name: "weighted rejects alpha",
			req: retrieval.SearchRequest{
				QueryText:     "alpha",
				QueryVector:   []float32{1, 0},
				TopK:          2,
				HybridMode:    retrieval.HybridWeighted,
				HybridOptions: retrieval.HybridOptions{Alpha: &alpha},
			},
		},
		{
			name: "convex rejects k",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridConvex,
				HybridOptions: retrieval.HybridOptions{
					K: 60,
				},
			},
		},
		{
			name: "convex rejects alpha and weights together",
			req: retrieval.SearchRequest{
				QueryText:   "alpha",
				QueryVector: []float32{1, 0},
				TopK:        2,
				HybridMode:  retrieval.HybridConvex,
				HybridOptions: retrieval.HybridOptions{
					Alpha: &alpha,
					Weights: map[retrieval.SearchSignal]float64{
						retrieval.SearchSignalBM25: 1,
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := idx.Search(ctx, ns, tc.req)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}

func testSparseHybridCapabilityMatchesSearch(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Sparse || !caps.Hybrid {
		t.Skip("Sparse=false or Hybrid=false")
	}
	if !caps.BM25 {
		t.Skip("sparse hybrid contract currently requires bm25+sparse")
	}
	ctx := context.Background()
	ns := "ns_sparse_hybrid"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "text", Content: "alpha", SparseVector: map[string]float32{"other": 1}, Timestamp: time.Now()},
		{ID: "sparse", Content: "beta", SparseVector: map[string]float32{"needle": 2}, Timestamp: time.Now().Add(time.Second)},
		{ID: "none", Content: "beta", SparseVector: map[string]float32{"other": 2}, Timestamp: time.Now().Add(2 * time.Second)},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText:  "alpha",
		SparseVec:  map[string]float32{"needle": 1},
		TopK:       3,
		HybridMode: retrieval.HybridRRF,
	})
	if err != nil {
		t.Fatalf("Sparse=true Hybrid=true backend rejected sparse hybrid search: %v", err)
	}
	ids := idsOf(resp.Hits)
	if !containsID(ids, "text") || !containsID(ids, "sparse") {
		t.Fatalf("sparse hybrid dropped a positive signal lane: %+v", resp.Hits)
	}
	if containsID(ids, "none") {
		t.Fatalf("sparse hybrid returned zero-evidence doc: %+v", resp.Hits)
	}
}

func testListWithVectorFalseDropsAllVectors(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_projection_vectors"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{
			ID:           "a",
			Content:      "alpha",
			Vector:       []float32{1, 0},
			SparseVector: map[string]float32{"alpha": 1},
			Timestamp:    time.Now(),
		},
	})
	resp, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 1, WithVector: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%+v", resp.Items)
	}
	if resp.Items[0].Vector != nil || resp.Items[0].SparseVector != nil {
		t.Fatalf("WithVector=false leaked vector payloads: %+v", resp.Items[0])
	}
}

func testDeleteByFilterValidation(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	d, ok := idx.(retrieval.DeletableByFilter)
	if !ok {
		t.Skip("not DeletableByFilter")
	}
	_, err := d.DeleteByFilter(context.Background(), "ns", retrieval.Filter{})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func testOptionalIterable(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	it, ok := idx.(retrieval.Iterable)
	if !ok {
		t.Skip("not Iterable")
	}
	ctx := context.Background()
	ns := "ns_iterable"
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "c", Content: "charlie", Metadata: map[string]any{"state": "original"}, Vector: []float32{1, 0}, SparseVector: map[string]float32{"c": 1}, Timestamp: time.Now()},
		{ID: "a", Content: "alpha", Metadata: map[string]any{"state": "original"}, Vector: []float32{1, 0}, SparseVector: map[string]float32{"a": 1}, Timestamp: time.Now().Add(time.Second)},
		{ID: "b", Content: "bravo", Metadata: map[string]any{"state": "original"}, Vector: []float32{1, 0}, SparseVector: map[string]float32{"b": 1}, Timestamp: time.Now().Add(2 * time.Second)},
	})
	first, next, err := it.Iterate(ctx, ns, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := docIDsOf(first); len(got) != 2 || got[0] != "a" || got[1] != "b" || next != "b" {
		t.Fatalf("first page ids=%v next=%q, want [a b] next b", got, next)
	}
	second, next, err := it.Iterate(ctx, ns, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := docIDsOf(second); len(got) != 1 || got[0] != "c" {
		t.Fatalf("second page ids=%v next=%q, want [c]", got, next)
	}
	if len(first) > 0 {
		first[0].Content = "mutated"
		if first[0].Metadata != nil {
			first[0].Metadata["state"] = "mutated"
		}
		if len(first[0].Vector) > 0 {
			first[0].Vector[0] = 99
		}
		if first[0].SparseVector != nil {
			first[0].SparseVector["a"] = 99
		}
		if g, ok := idx.(retrieval.DocGetter); ok {
			got, found, err := g.Get(ctx, ns, "a")
			if err != nil || !found {
				t.Fatalf("Get(a) found=%v err=%v", found, err)
			}
			if got.Content != "alpha" {
				t.Fatalf("Iterate returned aliased doc: %+v", got)
			}
			if got.Metadata != nil && got.Metadata["state"] != "original" {
				t.Fatalf("Iterate returned aliased metadata: %+v", got.Metadata)
			}
			if len(got.Vector) > 0 && got.Vector[0] != 1 {
				t.Fatalf("Iterate returned aliased vector: %+v", got.Vector)
			}
			if got.SparseVector != nil && got.SparseVector["a"] != 1 {
				t.Fatalf("Iterate returned aliased sparse vector: %+v", got.SparseVector)
			}
		}
	}
}

func testOptionalDroppable(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	d, ok := idx.(retrieval.Droppable)
	if !ok {
		t.Skip("not Droppable")
	}
	ctx := context.Background()
	ns := "ns_drop"
	mustUpsert(t, idx, ns, []retrieval.Doc{{ID: "a", Content: "alpha", Timestamp: time.Now()}})
	if err := d.Drop(ctx, ns); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 || resp.Total != 0 {
		t.Fatalf("List after Drop = %+v, want empty", resp)
	}
	mustUpsert(t, idx, ns, []retrieval.Doc{{ID: "b", Content: "bravo", Timestamp: time.Now()}})
	search, err := idx.Search(ctx, ns, retrieval.SearchRequest{QueryText: "bravo", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) != 1 || search.Hits[0].Doc.ID != "b" {
		t.Fatalf("Search after recreate = %+v, want b", search.Hits)
	}
}

func testCapabilitiesShape(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	c := retrieval.CapabilitiesOf(idx)
	if c.MaxListPageSize < 0 {
		t.Fatalf("invalid MaxListPageSize: %d", c.MaxListPageSize)
	}
	if c.NativeDeleteByFilter && !c.Extensions.DeleteByFilter {
		t.Fatalf("NativeDeleteByFilter requires callable DeleteByFilter: %+v", c)
	}
}

func idsOf(hits []retrieval.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Doc.ID)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func docIDsOf(docs []retrieval.Doc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out
}
