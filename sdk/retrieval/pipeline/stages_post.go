package pipeline

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// TimeDecay multiplies each hit's score by exp(-ln2·age/halfLife)
// (, §5.4). Reads/Writes: Final (or Fused if Final empty).
//
// HalfLife defaults to 30 days. Now allows tests to inject a clock; defaults to time.Now.
type TimeDecay struct {
	HalfLife time.Duration
	Now      func() time.Time
}

// Name implements Stage.
func (s TimeDecay) Name() string { return "TimeDecay" }

// Run implements Stage.
func (s TimeDecay) Run(_ context.Context, st *State) error {
	hl := s.HalfLife
	if hl <= 0 {
		hl = 30 * 24 * time.Hour
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	t := now()
	for i, h := range hits {
		if h.Doc.Timestamp.IsZero() {
			continue
		}
		age := t.Sub(h.Doc.Timestamp).Seconds()
		if age < 0 {
			age = 0
		}
		decay := math.Exp(-math.Ln2 * age / hl.Seconds())
		hits[i].Score = h.Score * decay
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["time_decay"] = decay
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}

// EntityBoost upweights hits whose Doc.Metadata["entities"] overlaps with QueryEntities
// . Reads: Fused or Final, QueryEntities. Writes: same slice.
//
// Boost is added per overlapping entity, capped at 1× score.
type EntityBoost struct {
	Boost float64
}

// Name implements Stage.
func (s EntityBoost) Name() string { return "EntityBoost" }

// Run implements Stage.
func (s EntityBoost) Run(_ context.Context, st *State) error {
	if len(st.QueryEntities) == 0 {
		return nil
	}
	boost := s.Boost
	if boost <= 0 {
		boost = 0.2
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	qSet := make(map[string]struct{}, len(st.QueryEntities))
	for _, e := range st.QueryEntities {
		qSet[strings.ToLower(strings.TrimSpace(e))] = struct{}{}
	}
	for i, h := range hits {
		ents := docEntities(h.Doc)
		var overlap int
		for _, e := range ents {
			if _, ok := qSet[strings.ToLower(strings.TrimSpace(e))]; ok {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		factor := 1 + boost*float64(overlap)
		if factor > 2 {
			factor = 2
		}
		hits[i].Score = h.Score * factor
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["entity_boost"] = factor
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}

func docEntities(d retrieval.Doc) []string {
	if d.Metadata == nil {
		return nil
	}
	v, ok := d.Metadata["entities"]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// PostFilter applies the request Filter on Final/Fused hits client-side
// . Used when the backend cannot push down some filter operators.
//
// Reads: Final/Fused, Request.Filter. Writes: Final.
type PostFilter struct{}

// Name implements Stage.
func (s PostFilter) Name() string { return "PostFilter" }

// Run implements Stage.
func (s PostFilter) Run(_ context.Context, st *State) error {
	if st.Request == nil {
		return nil
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	out := hits[:0]
	for _, h := range hits {
		if retrieval.DocMatchesFilter(h.Doc, st.Request.Filter) {
			out = append(out, h)
		}
	}
	st.Final = out
	return nil
}

// Dedup removes hits with duplicate Doc.ID, keeping highest score
// . Reads/Writes: Final.
type Dedup struct{}

// Name implements Stage.
func (s Dedup) Name() string { return "Dedup" }

// Run implements Stage.
func (s Dedup) Run(_ context.Context, st *State) error {
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	seen := make(map[string]int, len(hits))
	out := hits[:0]
	for _, h := range hits {
		if i, ok := seen[h.Doc.ID]; ok {
			if h.Score > out[i].Score {
				out[i] = h
			}
			continue
		}
		seen[h.Doc.ID] = len(out)
		out = append(out, h)
	}
	st.Final = out
	return nil
}

// Limit truncates Final to TopK and drops hits below MinScore
// . MUST be the last stage. Reads/Writes: Final.
type Limit struct {
	TopK     int
	MinScore float64
}

// Name implements Stage.
func (s Limit) Name() string { return "Limit" }

// Run implements Stage.
func (s Limit) Run(_ context.Context, st *State) error {
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	if s.MinScore != 0 {
		out := hits[:0]
		for _, h := range hits {
			if h.Score >= s.MinScore {
				out = append(out, h)
			}
		}
		hits = out
	}
	if s.TopK > 0 && len(hits) > s.TopK {
		hits = hits[:s.TopK]
	}
	st.Final = hits
	return nil
}

// pickFinalish returns Final if non-empty, otherwise lifts Fused/Reranked.
//
// When lifting from Reranked or Fused the slice is shallow-copied so that
// downstream stages (which freely mutate Score / Scores in place) cannot
// corrupt the original Reranked/Fused snapshots that diagnostics or later
// stages may still want to inspect. Hit.Doc remains shared by design — the
// document content itself is immutable from the pipeline's POV.
func pickFinalish(st *State) []retrieval.Hit {
	if len(st.Final) > 0 {
		return st.Final
	}
	if len(st.Reranked) > 0 {
		st.Final = cloneHits(st.Reranked)
		return st.Final
	}
	if len(st.Fused) > 0 {
		st.Final = cloneHits(st.Fused)
		return st.Final
	}
	return nil
}

func cloneHits(in []retrieval.Hit) []retrieval.Hit {
	out := make([]retrieval.Hit, len(in))
	copy(out, in)
	return out
}
