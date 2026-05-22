package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Project rebuilds the selected projection(s) from state.Facts.
type Project struct {
	fanout      *pipeline.Fanout
	projections []port.Projection
}

// NewProject constructs a Project stage.
func NewProject(fanout *pipeline.Fanout, projections []port.Projection) *Project {
	return &Project{fanout: fanout, projections: projections}
}

// Name implements pipeline.Stage.
func (Project) Name() string { return "project" }

// Run implements pipeline.Stage.
func (s *Project) Run(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	if state.ProjectionFilter != "" {
		return s.rebuildOne(ctx, state)
	}
	return s.rebuildAll(ctx, state)
}

func (s *Project) rebuildAll(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if err := s.fanout.RebuildRequired(ctx, state.Scope, state.Facts); err != nil {
		return diagnostic.RebuildProjectionDetail{}, err
	}
	s.fanout.RebuildOptional(ctx, state.Scope, state.Facts)
	n := len(state.Facts)
	state.PerProjection = append(state.PerProjection, rebuild.ProjectionRebuildResult{
		Name:          "all_required",
		PriorEntries:  n,
		Applied:       n,
		DriftDetected: false,
	})
	return diagnostic.RebuildProjectionDetail{
		ProjectionName: "all",
		Applied:        n,
		PriorEntries:   n,
		Latency:        time.Since(started),
	}, nil
}

func (s *Project) rebuildOne(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	var target port.Projection
	for _, p := range s.projections {
		if p != nil && p.Name() == state.ProjectionFilter {
			target = p
			break
		}
	}
	if target == nil {
		return diagnostic.RebuildProjectionDetail{}, errdefs.NotFoundf(
			"recall.RebuildProjection: projection %q not registered", state.ProjectionFilter)
	}
	started := time.Now()
	if err := target.Rebuild(ctx, state.Scope, state.Facts); err != nil {
		return diagnostic.RebuildProjectionDetail{
			ProjectionName: state.ProjectionFilter,
		}, fmt.Errorf("recall.RebuildProjection %q: %w", state.ProjectionFilter, err)
	}
	n := len(state.Facts)
	state.PerProjection = append(state.PerProjection, rebuild.ProjectionRebuildResult{
		Name:          state.ProjectionFilter,
		PriorEntries:  n,
		Applied:       n,
		DriftDetected: false,
	})
	return diagnostic.RebuildProjectionDetail{
		ProjectionName: state.ProjectionFilter,
		Applied:        n,
		PriorEntries:   n,
		Latency:        time.Since(started),
	}, nil
}

var _ pipeline.Stage[*rebuild.RebuildState] = (*Project)(nil)
