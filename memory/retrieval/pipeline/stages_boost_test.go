package pipeline

import (
	"context"
	"math"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/text/bm25"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
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

func TestBM25Boost_UsesCanonicalBM25ForCommonTerms(t *testing.T) {
	hits := []retrieval.Hit{
		{Doc: retrieval.Doc{ID: "short", Content: "coffee"}, Score: 0},
		{Doc: retrieval.Doc{ID: "repeat", Content: "coffee coffee"}, Score: 0},
		{Doc: retrieval.Doc{ID: "long", Content: "coffee with lots of unrelated filler words"}, Score: 0},
	}
	st := &State{
		Request: &retrieval.SearchRequest{QueryText: "coffee"},
		Final:   append([]retrieval.Hit(nil), hits...),
	}
	if err := (BM25Boost{Weight: 1}).Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	tok := tokenize.Detect("coffee")
	keywords := bm25.ExtractKeywords("coffee", tok)
	corpus := bm25.NewCorpus()
	docTokens := make(map[string][]string, len(hits))
	for _, h := range hits {
		tokens := tok.Tokenize(h.Doc.Content)
		docTokens[h.Doc.ID] = tokens
		corpus.AddDocument(tokens)
	}
	raw := make(map[string]float64, len(hits))
	minScore, maxScore := math.Inf(1), math.Inf(-1)
	for _, h := range hits {
		s := bm25.Score(docTokens[h.Doc.ID], keywords, corpus, bm25.WithK1(1.5))
		raw[h.Doc.ID] = s
		if s < minScore {
			minScore = s
		}
		if s > maxScore {
			maxScore = s
		}
	}
	if maxScore <= 0 || maxScore == minScore {
		t.Fatalf("fixture must produce positive differentiated BM25 scores: min=%v max=%v raw=%+v", minScore, maxScore, raw)
	}

	for _, h := range st.Final {
		want := (raw[h.Doc.ID] - minScore) / (maxScore - minScore)
		got := h.Scores["bm25_boost"]
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("%s bm25_boost=%v want %v (raw=%+v)", h.Doc.ID, got, want, raw)
		}
	}
}
