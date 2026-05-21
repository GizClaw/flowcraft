package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

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
		return diagnostic.RankDetail{InputCount: len(items), OutputCount: len(items)}, nil
	}
	started := time.Now()
	rankCap := state.Plan.TotalCap
	if s.hasReranker {
		rankCap = 0
	}
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
	return diagnostic.RankDetail{
		InputCount:             len(items),
		OutputCount:            len(out.Items),
		FinalCap:               state.Plan.TotalCap,
		BoostsApplied:          out.BoostsApplied,
		TimeDecayApplied:       out.TimeDecayApplied,
		SupersededDecayApplied: out.SupersededDecayApplied,
		Latency:                time.Since(started),
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Rank)(nil)
