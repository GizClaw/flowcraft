package pipeline

import (
	"context"
	"math"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

func TestBM25Boost_AllEqualScoresIsStable(t *testing.T) {
	st := &State{
		Request: &retrieval.SearchRequest{QueryText: "foo"},
		Final: []retrieval.Hit{
			{Doc: retrieval.Doc{ID: "a", Content: "foo"}, Score: 1},
			{Doc: retrieval.Doc{ID: "b", Content: "foo"}, Score: 1},
		},
	}
	if err := (BM25Boost{Weight: 0.3}).Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(st.Final) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(st.Final))
	}
	for _, h := range st.Final {
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			t.Fatalf("unexpected score for %s: %v", h.Doc.ID, h.Score)
		}
	}
}

func TestBM25Boost_NoQueryTermPresentIsNoOp(t *testing.T) {
	in := []retrieval.Hit{
		{Doc: retrieval.Doc{ID: "a", Content: "alpha"}, Score: 0.7},
		{Doc: retrieval.Doc{ID: "b", Content: "bravo"}, Score: 0.3},
	}
	st := &State{
		Request: &retrieval.SearchRequest{QueryText: "missing"},
		Final:   append([]retrieval.Hit(nil), in...),
	}
	if err := (BM25Boost{Weight: 0.5}).Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(st.Final) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(st.Final))
	}
	if st.Final[0].Score != in[0].Score || st.Final[1].Score != in[1].Score {
		t.Fatalf("unexpected score change: %+v -> %+v", in, st.Final)
	}
}
