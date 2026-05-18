package pipeline

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	base "github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// EntityLinkBoost upweights hits materialised by EntityLinkLookup.
type EntityLinkBoost struct {
	Boost float64
}

func (s EntityLinkBoost) Name() string { return "EntityLinkBoost" }

func (s EntityLinkBoost) Run(_ context.Context, st *base.State) error {
	if len(st.CandidateEntityIDs) == 0 {
		return nil
	}
	boost := s.Boost
	if boost <= 0 {
		boost = 0.3
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	idSet := make(map[string]struct{}, len(st.CandidateEntityIDs))
	for _, id := range st.CandidateEntityIDs {
		idSet[id] = struct{}{}
	}
	for i, h := range hits {
		if _, hit := idSet[h.Doc.ID]; !hit {
			continue
		}
		factor := 1 + boost
		if factor > 2 {
			factor = 2
		}
		hits[i].Score = h.Score * factor
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["entity_link_boost"] = factor
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}

// EntityBoost upweights hits whose entities metadata overlaps QueryEntities.
type EntityBoost struct {
	Boost float64
}

func (s EntityBoost) Name() string { return "EntityBoost" }

func (s EntityBoost) Run(_ context.Context, st *base.State) error {
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

// SlotCollapse keeps only the newest hit per recall slot_key.
type SlotCollapse struct{}

func (s SlotCollapse) Name() string { return "SlotCollapse" }

func (s SlotCollapse) Run(_ context.Context, st *base.State) error {
	hits := pickFinalish(st)
	if len(hits) < 2 {
		return nil
	}
	type keep struct {
		idx   int
		ts    time.Time
		score float64
	}
	bySlot := make(map[string]keep)
	drop := make(map[int]bool)
	for i, h := range hits {
		slot := retrieval.SlotKeyOf(h.Doc)
		if slot == "" {
			continue
		}
		cur, ok := bySlot[slot]
		if !ok {
			bySlot[slot] = keep{idx: i, ts: h.Doc.Timestamp, score: h.Score}
			continue
		}
		var newer bool
		switch {
		case h.Doc.Timestamp.After(cur.ts):
			newer = true
		case h.Doc.Timestamp.Equal(cur.ts) && h.Score > cur.score:
			newer = true
		}
		if newer {
			drop[cur.idx] = true
			bySlot[slot] = keep{idx: i, ts: h.Doc.Timestamp, score: h.Score}
		} else {
			drop[i] = true
		}
	}
	if len(drop) == 0 {
		return nil
	}
	out := hits[:0]
	for i, h := range hits {
		if !drop[i] {
			out = append(out, h)
		}
	}
	st.Final = out
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

func pickFinalish(st *base.State) []retrieval.Hit {
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
	return retrieval.CloneHits(in)
}
