package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Materialize is retained for tests that invoke it directly; the
// production read runner uses federation_fanout (D.5) which embeds
// materialization per sub-scope.
type Materialize struct {
	materializer port.Materializer
}

// NewMaterialize constructs a Materialize stage.
func NewMaterialize(materializer port.Materializer) *Materialize {
	return &Materialize{materializer: materializer}
}

// Name implements pipeline.Stage.
func (Materialize) Name() string { return "materialize" }

// Run implements pipeline.Stage.
func (s *Materialize) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	requested := 0
	returned := 0
	retired := 0
	for i := range state.SubScopeStates {
		sub := &state.SubScopeStates[i]
		requested += len(sub.Fused)
		items, drops, err := s.materializer.Materialize(ctx, sub.Fused)
		if err != nil {
			return diagnostic.MaterializeDetail{Requested: requested}, err
		}
		if !state.Query.IncludeRetired {
			before := len(items)
			items, drops = filterRetiredItems(items, drops, state.Now)
			retired += before - len(items)
		}
		sub.Materialized = items
		sub.MaterializeDrops = drops
		returned += len(items)
	}
	read.PromoteMergedItems(state)
	return diagnostic.MaterializeDetail{
		Requested:       requested,
		Returned:        returned,
		RetiredFiltered: retired,
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Materialize)(nil)
