package pipeline

import (
	"context"
	"math"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Reranker re-scores a candidate list against a query.
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []retrieval.Hit) ([]retrieval.Hit, error)
}

// Rerank invokes a Reranker on Fused (or Final) and writes Reranked
// . Reads: Fused or Final, Request.QueryText. Writes: Reranked.
type Rerank struct {
	Reranker Reranker
}

// Name implements Stage.
func (s Rerank) Name() string { return "Rerank" }

// Run implements Stage.
func (s Rerank) Run(ctx context.Context, st *State) error {
	if s.Reranker == nil {
		return nil
	}
	in := st.Final
	if len(in) == 0 {
		in = st.Fused
	}
	if len(in) == 0 || st.Request == nil {
		return nil
	}
	out, err := s.Reranker.Rerank(ctx, st.Request.QueryText, in)
	if err != nil {
		return err
	}
	st.Reranked = out
	st.Final = out
	return nil
}

// MMR enforces diversity via Maximal Marginal Relevance over Reranked or Final
// . Reads: Reranked or Final. Writes: Final.
//
// Lambda balances relevance vs. diversity; 1.0 = no diversity, 0 = max diversity.
type MMR struct {
	Lambda float64
	TopK   int
}

// Name implements Stage.
func (s MMR) Name() string { return "MMR" }

// Run implements Stage.
func (s MMR) Run(_ context.Context, st *State) error {
	in := st.Reranked
	if len(in) == 0 {
		in = st.Final
	}
	if len(in) == 0 {
		return nil
	}
	lambda := s.Lambda
	if lambda <= 0 {
		lambda = 0.7
	}
	k := s.TopK
	if k <= 0 || k > len(in) {
		k = len(in)
	}
	selected := make([]retrieval.Hit, 0, k)
	remaining := append([]retrieval.Hit(nil), in...)
	for len(selected) < k && len(remaining) > 0 {
		bestIdx := 0
		bestScore := -1e18
		for i, h := range remaining {
			rel := h.Score
			var simMax float64
			for _, sel := range selected {
				if sim := cosineHit(h, sel); sim > simMax {
					simMax = sim
				}
			}
			s := lambda*rel - (1-lambda)*simMax
			if s > bestScore {
				bestScore = s
				bestIdx = i
			}
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].Score > selected[j].Score })
	st.Final = selected
	return nil
}

func cosineHit(a, b retrieval.Hit) float64 {
	if len(a.Doc.Vector) == 0 || len(a.Doc.Vector) != len(b.Doc.Vector) {
		if a.Doc.ID == b.Doc.ID {
			return 1
		}
		return 0
	}
	var dot, na, nb float64
	for i := range a.Doc.Vector {
		dot += float64(a.Doc.Vector[i]) * float64(b.Doc.Vector[i])
		na += float64(a.Doc.Vector[i]) * float64(a.Doc.Vector[i])
		nb += float64(b.Doc.Vector[i]) * float64(b.Doc.Vector[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
