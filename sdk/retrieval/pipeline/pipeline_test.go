package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestPipelineBM25Only(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "alpha bravo", Timestamp: now},
		{ID: "b", Content: "charlie delta", Timestamp: now},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "alpha", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}

func TestPipelineMultiRetrieveAndRRF(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
		{ID: "2", Content: "unrelated", Vector: []float32{0, 1, 0}, Timestamp: time.Now()},
	})
	pipe := New(
		MultiRetrieve{
			"bm25":   {Mode: ModeBM25, TopK: 10},
			"vector": {Mode: ModeVector, TopK: 10},
		},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "coffee", QueryVector: []float32{1, 0, 0}, TopK: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 1 || resp.Hits[0].Doc.ID != "1" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}

func TestEntityBoost(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "alice loves coffee", Metadata: map[string]any{"entities": []string{"alice"}}, Timestamp: time.Now()},
		{ID: "b", Content: "bob loves coffee", Metadata: map[string]any{"entities": []string{"bob"}}, Timestamp: time.Now()},
	})
	pipe := New(
		EntityExtract{LLMExtractor: func(_ context.Context, _ string) ([]string, error) {
			return []string{"alice"}, nil
		}},
		MultiRetrieve{"bm25": {Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		EntityBoost{Boost: 0.5},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "loves coffee", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 2 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("expected a first, got %+v", resp.Hits)
	}
}

func TestTimeDecayBringsNewerToTop(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "old", Content: "user likes coffee", Timestamp: now.AddDate(0, -6, 0)},
		{ID: "new", Content: "user likes coffee", Timestamp: now.AddDate(0, 0, -1)},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		TimeDecay{HalfLife: 30 * 24 * time.Hour, Now: func() time.Time { return now }},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "coffee", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 2 || resp.Hits[0].Doc.ID != "new" {
		t.Fatalf("expected new on top, got %+v", resp.Hits)
	}
}

func TestDedupAndPostFilter(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "k", Content: "foo bar", Metadata: map[string]any{"keep": true}, Timestamp: now},
		{ID: "d", Content: "foo bar", Metadata: map[string]any{"keep": false}, Timestamp: now},
	})
	pipe := New(
		MultiRetrieve{
			"a": {Mode: ModeBM25, TopK: 10},
			"b": {Mode: ModeBM25, TopK: 10},
		},
		RRFFusion{K: 60},
		Dedup{},
		PostFilter{},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "foo",
		Filter:    retrieval.Filter{Eq: map[string]any{"keep": true}},
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "k" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}
