package pipeline

import (
	"context"
	"sort"

	base "github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// SupersededDecay penalizes hits whose metadata.superseded_by is set.
type SupersededDecay struct {
	Factor float64
}

func (s SupersededDecay) Name() string { return "SupersededDecay" }

func (s SupersededDecay) Run(_ context.Context, st *base.State) error {
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	f := s.Factor
	if f <= 0 {
		f = 0.3
	}
	for i, h := range hits {
		if h.Doc.Metadata == nil {
			continue
		}
		v, ok := h.Doc.Metadata["superseded_by"]
		if !ok {
			continue
		}
		if str, ok := v.(string); !ok || str == "" {
			continue
		}
		hits[i].Score = h.Score * f
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["superseded_decay"] = f
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}
