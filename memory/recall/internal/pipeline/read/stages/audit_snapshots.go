package stages

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

const (
	scoreLabelDiscovery       = "discovery_score"
	scoreLabelAssessment      = "assessment_relevance_score"
	scoreLabelRank            = "rank_score"
	scoreLabelFinal           = "final_score"
	scoreLabelContextPackRank = "context_pack_rank_score"
)

func snapshotsEnabled(state *read.ReadState) bool {
	return state != nil && state.Trace != nil
}

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
	return contextItemSnapshotsWithScoreLabel(items, scoreLabelDiscovery)
}

func contextItemSnapshotsWithScoreLabel(items []domain.ContextItem, label string) []diagnostic.CandidateSnapshot {
	return contextItemSnapshotsWithStateScoreLabel(nil, items, label)
}

func contextItemSnapshotsWithStateScoreLabel(state *read.ReadState, items []domain.ContextItem, label string) []diagnostic.CandidateSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(items))
	for _, item := range items {
		snap := candidateSnapshot(item.Candidate)
		applyContextItemScoreLabel(&snap, state, item, label)
		if snap.FactID == "" {
			snap.FactID = contextItemNodeID(item)
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
	return hitSnapshotsWithScoreLabel(hits, scoreLabelFinal)
}

func hitSnapshotsWithScoreLabel(hits []domain.Hit, label string) []diagnostic.CandidateSnapshot {
	if len(hits) == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(hits))
	for i, hit := range hits {
		snap := diagnostic.CandidateSnapshot{
			FactID:           contextPackTraceFactID(hit),
			Rank:             i + 1,
			ScoreLabel:       label,
			Source:           primaryHitSource(hit),
			Sources:          append([]string(nil), hit.Sources...),
			PrimarySource:    primaryHitSource(hit),
			ProjectionRoutes: append([]string(nil), hit.Sources...),
		}
		applySnapshotScore(&snap, label, hit.Score)
		snap.EvidenceIDs = contextPackTraceEvidenceIDs(hit)
		out = append(out, snap)
	}
	return out
}

func candidateSnapshot(c domain.Candidate) diagnostic.CandidateSnapshot {
	return diagnostic.CandidateSnapshot{
		FactID:         c.ID,
		Source:         c.Source,
		Rank:           c.Rank,
		ScoreLabel:     scoreLabelDiscovery,
		DiscoveryScore: c.Score,
		EvidenceIDs:    append([]string(nil), c.EvidenceIDs...),
		Sources:        candidateSources(c),
	}
}

func applyContextItemScoreLabel(snap *diagnostic.CandidateSnapshot, state *read.ReadState, item domain.ContextItem, label string) {
	if snap == nil {
		return
	}
	snap.ScoreLabel = label
	if env, ok := state.CandidateEnvelopeForItem(item); ok {
		snap.DiscoveryScore = env.DiscoveryScore
		if env.DiscoveryRank > 0 {
			snap.Rank = env.DiscoveryRank
		}
		if env.DiscoverySource != "" {
			snap.Source = env.DiscoverySource
		}
		snap.AssessmentScore = env.Assessment.RelevanceScore
		snap.RankScore = env.RankScore
	} else {
		if item.Ref.Score > 0 && item.Ref.Score != item.Candidate.Score {
			snap.DiscoveryScore = item.Ref.Score
		}
	}
	applySnapshotScore(snap, label, contextItemSnapshotScore(state, item, label))
}

func applySnapshotScore(snap *diagnostic.CandidateSnapshot, label string, score float64) {
	switch label {
	case scoreLabelDiscovery:
		snap.DiscoveryScore = score
	case scoreLabelAssessment:
		snap.AssessmentScore = score
	case scoreLabelRank, scoreLabelContextPackRank:
		snap.RankScore = score
	case scoreLabelFinal:
		snap.FinalScore = score
	}
}

func contextItemSnapshotScore(state *read.ReadState, item domain.ContextItem, label string) float64 {
	switch label {
	case scoreLabelDiscovery:
		if score, ok := state.CandidateDiscoveryScore(item); ok {
			return score
		}
		if item.Ref.Score > 0 && item.Ref.Score != item.Candidate.Score {
			return item.Ref.Score
		}
		return item.Candidate.Score
	case scoreLabelAssessment:
		if score, ok := state.CandidateAssessmentScore(item); ok {
			return score
		}
	case scoreLabelRank, scoreLabelContextPackRank, scoreLabelFinal:
		if score, ok := state.CandidateRankScore(item); ok {
			return score
		}
		return contextItemSnapshotScore(state, item, scoreLabelAssessment)
	}
	return 0
}

func contextItemNodeID(item domain.ContextItem) string {
	if item.Ref.ID != "" {
		return item.Ref.ID
	}
	if item.Candidate.ID != "" {
		return item.Candidate.ID
	}
	if item.Fact.ID != "" {
		return item.Fact.ID
	}
	if item.Observation.ID != "" {
		return item.Observation.ID
	}
	return item.Link.ID
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
