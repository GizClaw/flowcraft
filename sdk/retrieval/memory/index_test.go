package memory

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

func TestUpsertDeleteSearch(t *testing.T) {
	ctx := context.Background()
	idx := New()
	ns := "ltm:rt/r1/user/u1"
	d := retrieval.Doc{ID: "a1", Content: "user likes black coffee in morning", Timestamp: time.Now()}
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{d}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := idx.Get(ctx, ns, "a1")
	if err != nil || !ok || got.Content != d.Content {
		t.Fatalf("get: ok=%v err=%v got=%+v", ok, err, got)
	}
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{QueryText: "coffee", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a1" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
	if err := idx.Delete(ctx, ns, []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(ctx, ns, "a1"); ok {
		t.Fatal("expected miss after delete")
	}
}

func TestDeleteByFilterEmpty(t *testing.T) {
	idx := New()
	_, err := idx.DeleteByFilter(context.Background(), "ns", retrieval.Filter{})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation: %v", err)
	}
}

func TestDrop(t *testing.T) {
	ctx := context.Background()
	idx := New()
	_ = idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "x", Content: "hello"}})
	if err := idx.Drop(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(ctx, "ns", "x"); ok {
		t.Fatal("expected ns dropped")
	}
}

func TestListPagination(t *testing.T) {
	ctx := context.Background()
	idx := New()
	ns := "ns1"
	base := time.Unix(1700, 0).UTC()
	var docs []retrieval.Doc
	for i := 0; i < 5; i++ {
		docs = append(docs, retrieval.Doc{
			ID:        string(rune('a' + i)),
			Content:   "x",
			Timestamp: base.Add(time.Duration(i) * time.Hour),
		})
	}
	if err := idx.Upsert(ctx, ns, docs); err != nil {
		t.Fatal(err)
	}
	r1, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Items) != 2 || r1.NextPageToken == "" {
		t.Fatalf("r1=%+v", r1)
	}
	r2, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 2, PageToken: r1.NextPageToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Items) != 2 {
		t.Fatalf("r2 len=%d", len(r2.Items))
	}
}

func TestListPagination_StalePageTokenReturnsEmptyPage(t *testing.T) {
	ctx := context.Background()
	idx := New()
	ns := "ns-stale-token"
	now := time.Now()
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "x", Timestamp: now},
		{ID: "b", Content: "x", Timestamp: now.Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}

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

func TestHybridRRF(t *testing.T) {
	ctx := context.Background()
	idx := New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
		{ID: "2", Content: "unrelated", Vector: []float32{0, 1, 0}, Timestamp: time.Now()},
	})
	resp, err := idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryText: "coffee", QueryVector: []float32{1, 0, 0}, TopK: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 1 || resp.Hits[0].Doc.ID != "1" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}
