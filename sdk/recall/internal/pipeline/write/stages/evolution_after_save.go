package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// EvolutionAfterSave runs the post-Save best-effort evolution pass
// (reinforce / decay / repair). Errors are surfaced as a
// Status=failed diagnostic but never abort Save, mirroring legacy
// runEvolutionAfterSave behaviour.
//
// The stage implements Conditional so a nil EvolutionRunner is
// reported as Status=Skipped, which the legacy bridge correctly
// translates to "no OnPipeline event" — legacy code emitted nothing
// when evolution was unwired.
type EvolutionAfterSave struct {
	runner port.EvolutionRunner
}

// NewEvolutionAfterSave constructs the stage. runner may be nil.
func NewEvolutionAfterSave(runner port.EvolutionRunner) *EvolutionAfterSave {
	return &EvolutionAfterSave{runner: runner}
}

// Name implements pipeline.Stage.
func (EvolutionAfterSave) Name() string { return "evolution_after_save" }

// Skip implements pipeline.Conditional.
func (s *EvolutionAfterSave) Skip(_ context.Context, _ *write.WriteState) (bool, diagnostic.StageDetail) {
	if s.runner == nil {
		return true, diagnostic.EvolutionAfterSaveDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage. AfterSave errors are swallowed so
// Save outcome is unaffected; the err is left for the adapter to
// surface via the legacy OnPipeline channel by inspecting the
// returned StageDiagnostic.Err (set on the runner-level wrapper —
// see runner.go).
func (s *EvolutionAfterSave) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	if err := s.runner.AfterSave(ctx, state.Scope, state.AppendedFactIDs); err != nil {
		state.EvolutionErr = err
	}
	return diagnostic.EvolutionAfterSaveDetail{}, nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*EvolutionAfterSave)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*EvolutionAfterSave)(nil)
)
