package contract

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
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
	t.Run("ListPagination", func(t *testing.T) { testListPagination(t, f) })
	t.Run("ListPaginationStalePageToken", func(t *testing.T) { testListPaginationStalePageToken(t, f) })
	t.Run("FilterEqAndIn", func(t *testing.T) { testFilterEqIn(t, f) })
	t.Run("FilterRangeAndExists", func(t *testing.T) { testFilterRangeExists(t, f) })
	t.Run("FilterNotComposes", func(t *testing.T) { testFilterNotComposes(t, f) })
	t.Run("DeleteByFilterValidation", func(t *testing.T) { testDeleteByFilterValidation(t, f) })
	t.Run("CapabilitiesShape", func(t *testing.T) { testCapabilitiesShape(t, f) })
	t.Run("SearchDebugIncludeLanesPopulatesExecution", func(t *testing.T) { testSearchDebugIncludeLanesPopulatesExecution(t, f) })
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

func testListPagination(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	ctx := context.Background()
	ns := "ns_list"
	base := time.Unix(1700, 0).UTC()
	docs := make([]retrieval.Doc, 0, 5)
	for i := 0; i < 5; i++ {
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

func testCapabilitiesShape(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	c := idx.Capabilities()
	if c.MaxListPageSize < 0 {
		t.Fatalf("invalid MaxListPageSize: %d", c.MaxListPageSize)
	}
}

// testSearchDebugIncludeLanesPopulatesExecution validates that backends
// advertising Capabilities.Debug honour SearchRequest.Debug by populating
// SearchResponse.Execution. Backends that delegate execution to a higher
// layer (e.g. retrieval/pipeline) leave the flag false; this test then
// skips, keeping the Execution surface strictly opt-in for backends.
func testSearchDebugIncludeLanesPopulatesExecution(t *testing.T, f Factory) {
	idx, cleanup := f(t)
	defer cleanup()
	defer idx.Close()
	if !idx.Capabilities().Debug {
		t.Skip("backend does not advertise Capabilities.Debug")
	}
	ctx := context.Background()
	ns := "ns_debug"
	now := time.Now()
	mustUpsert(t, idx, ns, []retrieval.Doc{
		{ID: "a", Content: "alpha bravo", Timestamp: now},
		{ID: "b", Content: "charlie delta", Timestamp: now},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "alpha",
		TopK:      5,
		Debug:     retrieval.SearchDebug{IncludeLanes: true, IncludeStages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Execution == nil {
		t.Fatalf("expected SearchResponse.Execution when Capabilities.Debug=true, got %+v", resp)
	}
	exec := resp.Execution
	if len(exec.Lanes) == 0 && len(exec.Stages) == 0 {
		t.Fatal("expected at least one of Execution.Lanes / Execution.Stages to be populated")
	}
	hitIDs := make(map[string]struct{}, len(resp.Hits))
	for _, h := range resp.Hits {
		hitIDs[h.Doc.ID] = struct{}{}
	}
	for _, lane := range exec.Lanes {
		if lane.Key == "" {
			t.Fatalf("lane %d missing Key", len(exec.Lanes))
		}
		for _, h := range lane.Hits {
			if _, ok := hitIDs[h.Doc.ID]; !ok && len(resp.Hits) > 0 {
				// Lanes may legitimately surface candidates that fusion later
				// drops; only flag a lane hit that names a doc that is not in
				// the namespace at all.
				if h.Doc.ID != "a" && h.Doc.ID != "b" {
					t.Fatalf("lane %q surfaced unknown doc %q", lane.Key, h.Doc.ID)
				}
			}
		}
	}
}

func idsOf(hits []retrieval.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Doc.ID)
	}
	return out
}
