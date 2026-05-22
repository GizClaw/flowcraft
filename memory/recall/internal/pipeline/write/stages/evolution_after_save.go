package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// EvolutionAfterSave runs the post-Save best-effort evolution pass
// (reinforce / decay / repair). Errors are surfaced as a
// Status=failed diagnostic but never abort Save, mirroring legacy
// runEvolutionAfterSave behaviour.
//
// The stage implements Conditional so a nil EvolutionRunner is
// reported as Status=Skipped (no diagnostic Detail).
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
func (s *EvolutionAfterSave) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if s.runner == nil {
		return true, diagnostic.EvolutionAfterSaveDetail{}
	}
	// Async sync lane: episode-only (or policy-dropped) saves append no
	// structured semantic facts. Background worker runs AfterSave after
	// semantic derivation (§5.3).
	if state != nil && state.Mode == domain.WriteModeAsyncSemantic && len(state.AppendedFactIDs) == 0 {
		return true, diagnostic.EvolutionAfterSaveDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage. AfterSave is best-effort: the Save
// itself has already committed, so a runner failure must NOT abort
// the pipeline or trigger compensation. The error is wrapped via
// pipeline.BestEffort so the framework records the stage as
// Status=Degraded (Cluster C); state.EvolutionErr is kept populated
// for backward-compatible callers that read it directly.
func (s *EvolutionAfterSave) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	err := s.runner.AfterSave(ctx, state.Scope, state.AppendedFactIDs)
	if err != nil {
		state.EvolutionErr = err
	}
	return diagnostic.EvolutionAfterSaveDetail{}, pipeline.BestEffort(err)
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*EvolutionAfterSave)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*EvolutionAfterSave)(nil)
)
