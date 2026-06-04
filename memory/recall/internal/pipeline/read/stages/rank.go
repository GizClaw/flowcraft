package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const deterministicRankPoolMultiplier = 3

// Rank applies deterministic post-assessment ordering.
type Rank struct {
	ranker      port.Ranker
	hasReranker bool
}

// NewRank constructs a Rank stage.
func NewRank(ranker port.Ranker, hasReranker bool) *Rank {
	return &Rank{ranker: ranker, hasReranker: hasReranker}
}

// Name implements pipeline.Stage.
func (Rank) Name() string { return "rank" }

// Run implements pipeline.Stage.
func (s *Rank) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	if state == nil || !state.AssessmentApplied {
		if state != nil {
			state.Ranked = nil
		}
		return diagnostic.RankDetail{}, nil
	}
	items := append([]domain.ContextItem(nil), state.AssessedItems...)
	assessmentScores := assessmentScoresForItems(state, items)
	if s.ranker == nil || state.Plan == nil {
		state.Ranked = items
		recordRankScores(state, items, assessmentScores)
		detail := diagnostic.RankDetail{InputCount: len(items), OutputCount: len(items)}
		if snapshotsEnabled(state) {
			snaps := contextItemSnapshotsWithStateScoreLabel(state, items, scoreLabelRank)
			detail.Input = candidateSnapshotPtr(snaps)
			detail.Output = candidateSnapshotPtr(snaps)
		}
		return detail, nil
	}
	started := time.Now()
	rankCap := deterministicRankCap(state.Plan.TotalCap, s.hasReranker)
	intent := state.Plan.Intent
	if state.Intent != nil {
		intent = *state.Intent
	}
	out := s.ranker.Rank(ctx, port.RankInput{
		Items:            items,
		AssessmentScores: assessmentScores,
		Intent:           intent,
		FinalCap:         rankCap,
		Now:              state.Now,
	})
	state.Ranked = out.Items
	recordRankScores(state, out.Items, rankScoresForOutput(state, out))
	detail := diagnostic.RankDetail{
		InputCount:             len(items),
		OutputCount:            len(out.Items),
		FinalCap:               state.Plan.TotalCap,
		BoostsApplied:          out.BoostsApplied,
		TimeDecayApplied:       out.TimeDecayApplied,
		SupersededDecayApplied: out.SupersededDecayApplied,
		Latency:                time.Since(started),
	}
	if snapshotsEnabled(state) {
		detail.Input = candidateSnapshotPtr(contextItemSnapshotsWithStateScoreLabel(state, items, scoreLabelAssessment))
		detail.Output = candidateSnapshotPtr(contextItemSnapshotsWithStateScoreLabel(state, out.Items, scoreLabelRank))
	}
	return detail, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Rank)(nil)

func deterministicRankCap(finalCap int, hasReranker bool) int {
	if finalCap <= 0 || hasReranker {
		return 0
	}
	return finalCap * deterministicRankPoolMultiplier
}

func assessmentScoresForItems(state *read.ReadState, items []domain.ContextItem) []float64 {
	if len(items) == 0 {
		return nil
	}
	scores := make([]float64, len(items))
	for i, item := range items {
		if score, ok := state.CandidateAssessmentScore(item); ok {
			scores[i] = score
		}
	}
	return scores
}

func rankScoresForOutput(state *read.ReadState, out port.RankOutput) []float64 {
	if len(out.Items) == 0 {
		return nil
	}
	if len(out.RankScores) == len(out.Items) {
		return append([]float64(nil), out.RankScores...)
	}
	return assessmentScoresForItems(state, out.Items)
}

func recordRankScores(state *read.ReadState, items []domain.ContextItem, scores []float64) {
	if state == nil {
		return
	}
	for i, item := range items {
		score := 0.0
		if i < len(scores) {
			score = scores[i]
		}
		state.RecordCandidateRank(item, score)
	}
}
