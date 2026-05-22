package stages

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

func candidateSnapshots(candidates []domain.Candidate) []diagnostic.CandidateSnapshot {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, candidateSnapshot(c))
	}
	return out
}

func candidateSnapshotPtr(in []diagnostic.CandidateSnapshot) *[]diagnostic.CandidateSnapshot {
	if len(in) == 0 {
		return nil
	}
	return &in
}

func appendSnapshotPtr(dst *[]diagnostic.CandidateSnapshot, in []diagnostic.CandidateSnapshot) *[]diagnostic.CandidateSnapshot {
	if len(in) == 0 {
		return dst
	}
	if dst == nil {
		out := append([]diagnostic.CandidateSnapshot(nil), in...)
		return &out
	}
	*dst = append(*dst, in...)
	return dst
}

func contextItemSnapshots(items []domain.ContextItem) []diagnostic.CandidateSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(items))
	for _, item := range items {
		snap := candidateSnapshot(item.Candidate)
		if snap.FactID == "" {
			snap.FactID = item.Fact.ID
		}
		if len(snap.EvidenceIDs) == 0 {
			for _, ref := range item.Fact.EvidenceRefs {
				if ref.ID != "" {
					snap.EvidenceIDs = append(snap.EvidenceIDs, ref.ID)
				}
			}
		}
		out = append(out, snap)
	}
	return out
}

func hitSnapshots(hits []domain.Hit) []diagnostic.CandidateSnapshot {
	if len(hits) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(hits))
	for i, hit := range hits {
		snap := diagnostic.CandidateSnapshot{
			FactID:  hit.Fact.ID,
			Rank:    i + 1,
			Score:   hit.Score,
			Sources: append([]string(nil), hit.Sources...),
		}
		for _, ref := range hit.Fact.EvidenceRefs {
			if ref.ID != "" {
				snap.EvidenceIDs = append(snap.EvidenceIDs, ref.ID)
			}
		}
		out = append(out, snap)
	}
	return out
}

func candidateSnapshot(c domain.Candidate) diagnostic.CandidateSnapshot {
	return diagnostic.CandidateSnapshot{
		FactID:      c.FactID,
		Source:      c.Source,
		Rank:        c.Rank,
		Score:       c.Score,
		EvidenceIDs: append([]string(nil), c.EvidenceIDs...),
		Sources:     candidateSources(c),
	}
}

func candidateSources(c domain.Candidate) []string {
	if c.Metadata != nil {
		if sources, ok := c.Metadata["sources"].([]string); ok && len(sources) > 0 {
			return append([]string(nil), sources...)
		}
	}
	if c.Source == "" {
		return nil
	}
	return []string{c.Source}
}
