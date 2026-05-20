package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
)

// RankItemsFunc is the rank-boost delegate (sdk/recall.rankContextItems).
type RankItemsFunc func(items []domain.ContextItem, intent domain.QueryIntent, finalCap int) []domain.ContextItem

// Rank applies the deterministic post-materialize ranker.
type Rank struct {
	rank RankItemsFunc
	// HasReranker mirrors memory.reranker != nil: defer TotalCap so
	// the optional reranker sees the widest fused pool.
	hasReranker bool
}

// NewRank constructs a Rank stage.
func NewRank(rank RankItemsFunc, hasReranker bool) *Rank {
	return &Rank{rank: rank, hasReranker: hasReranker}
}

// Name implements pipeline.Stage.
func (Rank) Name() string { return "rank" }

// Run implements pipeline.Stage.
func (s *Rank) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	_ = ctx
	read.PromoteMergedItems(state)
	items := state.MergedItems
	state.AfterTrust = items
	if s.rank == nil || state.Plan == nil {
		state.Ranked = items
		return diagnostic.RankDetail{InputCount: len(items), OutputCount: len(items)}, nil
	}
	started := time.Now()
	rankCap := state.Plan.TotalCap
	if s.hasReranker {
		rankCap = 0
	}
	ranked := s.rank(items, state.Plan.Intent, rankCap)
	state.Ranked = ranked
	if state.Trace != nil {
		state.Trace.Materialized = len(ranked)
	}
	return diagnostic.RankDetail{
		InputCount:  len(items),
		OutputCount: len(ranked),
		FinalCap:    state.Plan.TotalCap,
		Latency:     time.Since(started),
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Rank)(nil)
