package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ProjectRequired drives the Required-consistency projection fanout.
// A required-projection failure must abort Save with strict
// transactional semantics; the stage performs its own forget-required
// + forget-optional self-cleanup before returning the error so the
// leaking partial-Project mid-stage cannot reach a later RebuildAll.
//
// The Compensator handles cleanup when a stage AFTER project_required
// fails — today the chain ends here (project_optional + evolution
// never fail), but plan §6 calls the compensator out for future-
// proofing and runner-level tests cover the unreachable-today branch.
type ProjectRequired struct {
	fanout *pipeline.Fanout
	hook   port.TelemetryHook
}

// NewProjectRequired constructs the stage.
func NewProjectRequired(fanout *pipeline.Fanout, hook port.TelemetryHook) *ProjectRequired {
	return &ProjectRequired{fanout: fanout, hook: hook}
}

// Name implements pipeline.Stage.
func (ProjectRequired) Name() string { return "project_required" }

// Skip implements pipeline.Conditional.
func (ProjectRequired) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if asyncStructuredLegInactive(state) {
		return true, diagnostic.ProjectDetail{Consistency: port.Required.String()}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (s *ProjectRequired) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	err := s.fanout.ProjectRequired(ctx, state.Resolution.Facts)
	detail := diagnostic.ProjectDetail{
		Consistency: port.Required.String(),
	}
	if err != nil {
		s.selfCleanup(ctx, state)
		state.FailedStage = "project_required"
		_ = time.Since(started)
		return detail, err
	}
	state.RequiredApplied = len(state.Resolution.Facts)
	return detail, nil
}

// Compensate implements pipeline.Compensator. The behaviour mirrors
// project_required's self-cleanup path so any future stage added
// between project_required and project_optional / evolution gets
// proper rollback for free.
func (s *ProjectRequired) Compensate(ctx context.Context, state *write.WriteState) error {
	s.selfCleanup(ctx, state)
	return nil
}

// selfCleanup undoes the Project work this stage did (or might have
// done partially) via fanout forget paths (required + optional,
// including the evidence lens when registered).
func (s *ProjectRequired) selfCleanup(ctx context.Context, state *write.WriteState) {
	if len(state.AppendedFactIDs) == 0 {
		return
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	_ = s.fanout.ForgetRequired(cleanupCtx, state.Scope, state.AppendedFactIDs)
	s.fanout.ForgetOptional(cleanupCtx, state.Scope, state.AppendedFactIDs)
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*ProjectRequired)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*ProjectRequired)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*ProjectRequired)(nil)
)
