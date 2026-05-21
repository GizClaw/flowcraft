package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// EvolutionAfterRecall runs the post-Recall best-effort evolution
// pass. Errors are non-fatal and surfaced through the stage's
// StageDiagnostic.Err (Phase E.3: Stages-only).
type EvolutionAfterRecall struct {
	runner port.EvolutionRunner
}

// NewEvolutionAfterRecall constructs the stage. runner may be nil.
func NewEvolutionAfterRecall(runner port.EvolutionRunner) *EvolutionAfterRecall {
	return &EvolutionAfterRecall{runner: runner}
}

// Name implements pipeline.Stage.
func (EvolutionAfterRecall) Name() string { return "evolution_after_recall" }

// Skip implements pipeline.Conditional.
func (s *EvolutionAfterRecall) Skip(_ context.Context, _ *read.ReadState) (bool, diagnostic.StageDetail) {
	if s.runner == nil {
		return true, diagnostic.EvolutionAfterRecallDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage. AfterRecall is best-effort: the
// Recall result has already been materialised, so a runner failure
// must NOT abort the pipeline or trigger compensation. The error is
// wrapped via pipeline.BestEffort so the framework records the stage
// as Status=Degraded (Cluster C); state.EvolutionErr is kept
// populated for backward-compatible callers that read it directly.
func (s *EvolutionAfterRecall) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	trace := read.PublicRecallTrace(state)
	err := s.runner.AfterRecall(ctx, state.Scope, trace)
	if err != nil {
		state.EvolutionErr = err
	}
	return diagnostic.EvolutionAfterRecallDetail{}, pipeline.BestEffort(err)
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*EvolutionAfterRecall)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*EvolutionAfterRecall)(nil)
)
