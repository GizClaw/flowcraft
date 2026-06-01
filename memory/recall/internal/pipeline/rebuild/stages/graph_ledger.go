package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// GraphLedger rebuilds the experimental canonical graph from TemporalFacts.
// It only runs during full rebuilds; single projection rebuilds intentionally
// leave canonical graph stores untouched.
type GraphLedger struct {
	observations port.ObservationStore
	links        port.LinkStore
	projection   port.ObservationProjection
}

func NewGraphLedger(observations port.ObservationStore, links port.LinkStore, projection port.ObservationProjection) *GraphLedger {
	return &GraphLedger{observations: observations, links: links, projection: projection}
}

func (GraphLedger) Name() string { return "graph_ledger" }

func (s *GraphLedger) Skip(_ context.Context, state *rebuild.RebuildState) (bool, diagnostic.StageDetail) {
	if s == nil || s.observations == nil || s.links == nil {
		return true, diagnostic.RebuildGraphDetail{}
	}
	if state == nil || state.ProjectionFilter != "" {
		return true, diagnostic.RebuildGraphDetail{}
	}
	return false, nil
}

func (s *GraphLedger) Run(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	started := time.Now()
	delta := graphledger.BuildDelta(state.Scope, state.Facts, nil, nil, time.Time{}, started, "")

	if _, err := s.links.DeleteByScope(ctx, state.Scope); err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph links clear: %w", err)
	}
	if _, err := s.observations.DeleteByScope(ctx, state.Scope); err != nil {
		return diagnostic.RebuildGraphDetail{Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph observations clear: %w", err)
	}
	if err := s.observations.Append(ctx, delta.Observations); err != nil {
		return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph observations append: %w", err)
	}
	if s.projection != nil {
		if err := s.projection.RebuildObservations(ctx, state.Scope, delta.Observations); err != nil {
			return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
				fmt.Errorf("recall.RebuildAll: observation projection rebuild: %w", err)
		}
	}
	if err := s.links.Append(ctx, delta.Links); err != nil {
		return diagnostic.RebuildGraphDetail{Observations: len(delta.Observations), Links: len(delta.Links), Latency: time.Since(started)},
			fmt.Errorf("recall.RebuildAll: graph links append: %w", err)
	}

	return diagnostic.RebuildGraphDetail{
		Observations: len(delta.Observations),
		Links:        len(delta.Links),
		Latency:      time.Since(started),
	}, nil
}

var (
	_ pipeline.Stage[*rebuild.RebuildState]       = (*GraphLedger)(nil)
	_ pipeline.Conditional[*rebuild.RebuildState] = (*GraphLedger)(nil)
)
