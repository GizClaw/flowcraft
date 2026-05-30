package pipeline

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/scoring"
)

// RRFFusion fuses Recalls into Fused via Reciprocal Rank Fusion.
// Reads: Recalls. Writes: Fused. K defaults to 60. Algorithm core
// lives in [scoring.RRF]; this Stage adapts the lane map to a
// rank-list slice.
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
	lists := make([][]retrieval.Hit, 0, len(st.Recalls))
	for _, lane := range sortedRecallNames(st.Recalls) {
		lists = append(lists, st.Recalls[lane])
	}
	st.Fused = scoring.RRF(lists, s.K)
	return nil
}

// WeightedFusion combines lanes via per-lane weights after min-max
// normalization. Reads: Recalls. Writes: Fused. Algorithm core lives
// in [scoring.WeightedFusion]; this Stage forwards the lane map.
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
	lanes := make(map[string][]retrieval.Hit, len(st.Recalls))
	for lane, hits := range st.Recalls {
		lanes[lane] = hits
	}
	st.Fused = scoring.WeightedFusion(lanes, s.Weights)
	return nil
}

// ConvexFusion combines exactly two lanes (bm25/vector) as α·bm25 + (1-α)·vector
// after min-max normalization.
// Reads: Recalls[retrieval.LaneBM25], Recalls[retrieval.LaneVector]. Writes: Fused.
type ConvexFusion struct {
	Alpha float64 // weight for bm25 lane; 0..1
}

// Name implements Stage.
func (s ConvexFusion) Name() string { return "ConvexFusion" }

// Run implements Stage.
func (s ConvexFusion) Run(ctx context.Context, st *State) error {
	a := s.Alpha
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	wf := WeightedFusion{Weights: map[string]float64{
		string(retrieval.LaneBM25):   a,
		string(retrieval.LaneVector): 1 - a,
	}}
	return wf.Run(ctx, st)
}

func sortedRecallNames(recalls map[string][]retrieval.Hit) []string {
	names := make([]string, 0, len(recalls))
	for name := range recalls {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
