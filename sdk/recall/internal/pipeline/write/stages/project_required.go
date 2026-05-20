package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
)

// ProjectRequired drives the Required-consistency projection fanout.
// A required-projection failure must abort Save with strict
// transactional semantics; the stage performs its own forget-required
// + forget-optional + evidence-forget self-cleanup before returning
// the error so the leaking partial-Project mid-stage cannot reach a
// later RebuildAll.
//
// The Compensator handles cleanup when a stage AFTER project_required
// fails — today the chain ends here (project_optional + evolution
// never fail), but plan §6 calls the compensator out for future-
// proofing and runner-level tests cover the unreachable-today branch.
type ProjectRequired struct {
	fanout        *projection.Fanout
	evidenceStore port.EvidenceStore
	hook          port.TelemetryHook
}

// NewProjectRequired constructs the stage. evidenceStore may be nil.
func NewProjectRequired(fanout *projection.Fanout, evidenceStore port.EvidenceStore, hook port.TelemetryHook) *ProjectRequired {
	return &ProjectRequired{fanout: fanout, evidenceStore: evidenceStore, hook: hook}
}

// Name implements pipeline.Stage.
func (ProjectRequired) Name() string { return "project_required" }

// Run implements pipeline.Stage.
func (s *ProjectRequired) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	err := s.fanout.ProjectRequired(ctx, state.Resolution.Facts)
	detail := diagnostic.ProjectDetail{
		Consistency: projection.Required.String(),
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
// done partially) plus the upstream evidence mirror, matching legacy
// rollbackSave's first three steps byte-for-byte.
func (s *ProjectRequired) selfCleanup(ctx context.Context, state *write.WriteState) {
	if len(state.AppendedFactIDs) == 0 {
		return
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if err := s.fanout.ForgetRequired(cleanupCtx, state.Scope, state.AppendedFactIDs); err != nil {
		s.emit(port.ProjectionEvent{
			Projection:  "save_rollback.forget_required",
			Op:          port.OpForget,
			Consistency: projection.Required.String(),
			FactCount:   len(state.AppendedFactIDs),
			Err:         fmt.Errorf("rollback cleanup: %w", err),
		})
	}
	s.fanout.ForgetOptional(cleanupCtx, state.Scope, state.AppendedFactIDs)
	if s.evidenceStore != nil {
		if err := s.evidenceStore.ForgetByFact(cleanupCtx, state.Scope, state.AppendedFactIDs); err != nil {
			s.emit(port.ProjectionEvent{
				Projection:  "save_rollback.evidence_forget",
				Op:          port.OpForget,
				Consistency: projection.Required.String(),
				FactCount:   len(state.AppendedFactIDs),
				Err:         fmt.Errorf("rollback cleanup: %w", err),
			})
		}
	}
}

func (s *ProjectRequired) emit(ev port.ProjectionEvent) {
	if s.hook == nil {
		return
	}
	s.hook.OnProjection(ev)
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*ProjectRequired)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*ProjectRequired)(nil)
)
