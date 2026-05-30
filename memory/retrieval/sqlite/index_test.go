package sqlite_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/contract"

	sqlx "github.com/GizClaw/flowcraft/memory/retrieval/sqlite"
)

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) (retrieval.Index, func()) {
		dir := t.TempDir()
		idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
		if err != nil {
			t.Fatal(err)
		}
		return idx, func() {}
	})
}

func TestSearchSelectiveFilterBeyondInitialWindow(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	docs := make([]retrieval.Doc, 0, 80)
	for i := 0; i < 79; i++ {
		docs = append(docs, retrieval.Doc{
			ID:       "common-" + strconv.Itoa(i),
			Content:  "alpha alpha alpha common",
			Metadata: map[string]any{"tenant": "common"},
		})
	}
	docs = append(docs, retrieval.Doc{
		ID:       "rare",
		Content:  "alpha rare",
		Metadata: map[string]any{"tenant": "rare"},
	})
	if err := idx.Upsert(t.Context(), "ns_selective", docs); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.Search(t.Context(), "ns_selective", retrieval.SearchRequest{
		QueryText: "alpha",
		TopK:      1,
		Filter:    retrieval.Filter{Eq: map[string]any{"tenant": "rare"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "rare" {
		t.Fatalf("hits = %+v, want rare", resp.Hits)
	}
}

func TestSearchHybridUsesConsistentScore(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(t.Context(), "ns_hybrid", []retrieval.Doc{
		{ID: "a", Content: "alpha", Vector: []float32{1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := idx.Search(t.Context(), "ns_hybrid", retrieval.SearchRequest{
		QueryText:   "alpha",
		QueryVector: []float32{1, 0},
		TopK:        1,
		MinScore:    0.9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 {
		t.Fatalf("hybrid final score should satisfy MinScore, hits=%+v", resp.Hits)
	}
	got := resp.Hits[0]
	if got.Score != got.Scores["bm25"]+got.Scores["cos"] {
		t.Fatalf("Hit.Score=%v, bm25+cos=%v", got.Score, got.Scores["bm25"]+got.Scores["cos"])
	}
}

func TestListAndCountPushDownCompoundFilter(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(t.Context(), "ns_filter_dsl", []retrieval.Doc{
		{ID: "a", Content: "alpha", Metadata: map[string]any{
			"tenant": "acme", "status": "active", "score": 10, "tier": 1, "kind": "fable",
		}},
		{ID: "b", Content: "bravo", Metadata: map[string]any{
			"tenant": "acme", "status": "archived", "score": 3, "tier": 2, "kind": "fable",
		}},
		{ID: "c", Content: "charlie", Metadata: map[string]any{
			"tenant": "other", "status": "active", "score": 7, "tier": 3, "kind": "tech",
		}},
		{ID: "d", Content: "delta", Metadata: map[string]any{
			"tenant": "acme", "status": nil, "tier": 1,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	filter := retrieval.Filter{
		Eq:      map[string]any{"tenant": "acme"},
		Neq:     map[string]any{"status": "archived"},
		In:      map[string][]any{"tier": {1, 3}},
		NotIn:   map[string][]any{"status": {"deleted"}},
		Range:   map[string]retrieval.Range{"score": {Gte: int32(5), Lt: uint(11)}},
		Exists:  []string{"kind"},
		Missing: []string{"deleted_at"},
	}
	resp, err := idx.List(t.Context(), "ns_filter_dsl", retrieval.ListRequest{
		Filter:  filter,
		OrderBy: retrieval.OrderByIDAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "a" || resp.Total != 1 {
		t.Fatalf("List = %+v, want only a", resp)
	}
	c, ok := any(idx).(retrieval.Countable)
	if !ok {
		t.Fatal("sqlite index should implement Countable")
	}
	n, err := c.Count(t.Context(), "ns_filter_dsl", filter)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
}

func TestListPushDownBooleanOrNotFilter(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(t.Context(), "ns_bool_dsl", []retrieval.Doc{
		{ID: "a", Metadata: map[string]any{"tenant": "acme", "status": "active"}},
		{ID: "b", Metadata: map[string]any{"tenant": "acme", "status": "archived"}},
		{ID: "c", Metadata: map[string]any{"tenant": "other", "status": "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	filter := retrieval.Filter{
		Or: []retrieval.Filter{
			{Eq: map[string]any{"tenant": "other"}},
			{Not: &retrieval.Filter{Eq: map[string]any{"status": "archived"}}},
		},
	}
	resp, err := idx.List(t.Context(), "ns_bool_dsl", retrieval.ListRequest{
		Filter:  filter,
		OrderBy: retrieval.OrderByIDAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(resp.Items))
	for _, d := range resp.Items {
		got = append(got, d.ID)
	}
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func TestDrop(t *testing.T) {
	dir := t.TempDir()
	idx, err := sqlx.Open(filepath.Join(dir, "fc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(t.Context(), "ns_drop", []retrieval.Doc{{ID: "x", Content: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Drop(t.Context(), "ns_drop"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(t.Context(), "ns_drop", "x"); ok {
		t.Fatal("expected ns dropped")
	}
}
