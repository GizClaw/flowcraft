package pipeline

import (
	"context"
	"math"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// RRFFusion fuses Recalls into Fused via Reciprocal Rank Fusion.
// Reads: Recalls. Writes: Fused. K defaults to 60.
type RRFFusion struct {
	K float64
}

// Name implements Stage.
func (s RRFFusion) Name() string { return "RRFFusion" }

// Run implements Stage.
func (s RRFFusion) Run(_ context.Context, st *State) error {
	if len(st.Recalls) == 0 {
		return nil
	}
	k := s.K
	if k <= 0 {
		k = 60
	}
	scores := map[string]float64{}
	docs := map[string]retrieval.Hit{}
	for _, hits := range st.Recalls {
		for rank, h := range hits {
			scores[h.Doc.ID] += 1.0 / (k + float64(rank+1))
			if cur, ok := docs[h.Doc.ID]; !ok || h.Score > cur.Score {
				docs[h.Doc.ID] = h
			}
		}
	}
	out := make([]retrieval.Hit, 0, len(scores))
	for id, s := range scores {
		h := docs[id]
		h.Score = s
		if h.Scores == nil {
			h.Scores = map[string]float64{}
		}
		h.Scores["rrf"] = s
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	st.Fused = out
	return nil
}

// WeightedFusion combines lanes via per-lane weights after min-max normalization
// . Reads: Recalls. Writes: Fused.
//
// Missing weights default to 1.0.
type WeightedFusion struct {
	Weights map[string]float64
}

// Name implements Stage.
func (s WeightedFusion) Name() string { return "WeightedFusion" }

// Run implements Stage.
func (s WeightedFusion) Run(_ context.Context, st *State) error {
	if len(st.Recalls) == 0 {
		return nil
	}
	totals := map[string]float64{}
	docs := map[string]retrieval.Hit{}
	for lane, hits := range st.Recalls {
		w := 1.0
		if v, ok := s.Weights[lane]; ok {
			w = v
		}
		var min, max float64 = math.MaxFloat64, -math.MaxFloat64
		for _, h := range hits {
			if h.Score < min {
				min = h.Score
			}
			if h.Score > max {
				max = h.Score
			}
		}
		span := max - min
		for _, h := range hits {
			n := 0.0
			if span > 0 {
				n = (h.Score - min) / span
			} else if max > 0 {
				n = 1
			}
			totals[h.Doc.ID] += w * n
			if cur, ok := docs[h.Doc.ID]; !ok || h.Score > cur.Score {
				docs[h.Doc.ID] = h
			}
		}
	}
	out := make([]retrieval.Hit, 0, len(totals))
	for id, t := range totals {
		h := docs[id]
		h.Score = t
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	st.Fused = out
	return nil
}

// ConvexFusion combines exactly two lanes (bm25/vector) as α·bm25 + (1-α)·vector
// after min-max normalization.
// Reads: Recalls["bm25"], Recalls["vector"]. Writes: Fused.
type ConvexFusion struct {
	Alpha float64 // weight for bm25 lane; 0..1
}

// Name implements Stage.
func (s ConvexFusion) Name() string { return "ConvexFusion" }

// Run implements Stage.
func (s ConvexFusion) Run(_ context.Context, st *State) error {
	a := s.Alpha
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	wf := WeightedFusion{Weights: map[string]float64{"bm25": a, "vector": 1 - a}}
	return wf.Run(context.Background(), st)
}
