package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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
	var aggregated []diagnostic.CandidateDrop
	for i := range state.SubScopeStates {
		sub := &state.SubScopeStates[i]
		requested += len(sub.Fused)
		items, drops, err := s.materializer.Materialize(ctx, sub.Fused)
		if err != nil {
			// Cluster F (2026-05-21): publish whatever drops we
			// gathered so far on state before returning; the
			// diagnostic detail still mirrors Requested as before.
			state.MaterializeDrops = aggregated
			return diagnostic.MaterializeDetail{Requested: requested}, err
		}
		if !state.Query.IncludeRetired {
			before := len(items)
			items, drops = filterRetiredItems(items, drops, state.Now)
			retired += before - len(items)
		}
		sub.Materialized = items
		sub.MaterializeDrops = drops
		aggregated = append(aggregated, drops...)
		returned += len(items)
	}
	// Cluster F (2026-05-21): MaterializeDrops on ReadState is the
	// authoritative inter-stage channel — write it BEFORE returning
	// the diagnostic detail so downstream stages (notably
	// evolution_after_recall) never have to reach into Trace.Stages.
	// The diagnostic detail still carries the same counters so trace
	// visibility is unchanged.
	state.MaterializeDrops = aggregated
	read.PromoteMergedItems(state)
	return diagnostic.MaterializeDetail{
		Requested:       requested,
		Returned:        returned,
		RetiredFiltered: retired,
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Materialize)(nil)
