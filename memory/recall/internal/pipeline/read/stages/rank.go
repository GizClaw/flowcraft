package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const deterministicRankPoolMultiplier = 3

// Rank applies the deterministic post-materialize ranker (Phase E.1).
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
	items := state.AfterTrust
	if len(items) == 0 {
		read.PromoteMergedItems(state)
		items = state.MergedItems
	}
	if s.ranker == nil || state.Plan == nil {
		state.Ranked = items
		detail := diagnostic.RankDetail{InputCount: len(items), OutputCount: len(items)}
		if snapshotsEnabled(state) {
			snaps := contextItemSnapshots(items)
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
		Items:    items,
		Intent:   intent,
		FinalCap: rankCap,
		Now:      state.Now,
	})
	state.Ranked = out.Items
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
		detail.Input = candidateSnapshotPtr(contextItemSnapshots(items))
		detail.Output = candidateSnapshotPtr(contextItemSnapshots(out.Items))
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
