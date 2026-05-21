package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ProjectOptional drives the Optional-consistency projection fanout.
// Optional failures are non-fatal by design: fanout.ProjectOptional
// already emits per-projection telemetry and never returns an error,
// so this stage records a Status=ok diagnostic regardless of
// individual projection outcomes. There is intentionally NO
// Compensator — Optional projections are best-effort by definition
// and rolling them back would be a no-op surface area.
//
// The stage implements Conditional so that an empty resolution
// (HasWork==false) reports Status=Skipped, matching what legacy code
// effectively did via the early-return short-circuit.
type ProjectOptional struct {
	fanout *pipeline.Fanout
}

// NewProjectOptional constructs the stage.
func NewProjectOptional(fanout *pipeline.Fanout) *ProjectOptional {
	return &ProjectOptional{fanout: fanout}
}

// Name implements pipeline.Stage.
func (ProjectOptional) Name() string { return "project_optional" }

// Skip implements pipeline.Conditional.
func (s *ProjectOptional) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if !state.HasWork() {
		return true, diagnostic.ProjectDetail{Consistency: port.Optional.String()}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (s *ProjectOptional) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	s.fanout.ProjectOptional(ctx, state.Resolution.Facts)
	state.OptionalApplied = len(state.Resolution.Facts)
	return diagnostic.ProjectDetail{Consistency: port.Optional.String()}, nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*ProjectOptional)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*ProjectOptional)(nil)
)
