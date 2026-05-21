package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
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
//
// Cluster F (2026-05-21): the stage's inter-stage signal is the
// canonical State, not the diagnostic Trace. We read materialize
// drops via state.CollectMaterializeDrops() and surface them to the
// runner as a synthetic single-stage RecallTrace shaped exactly the
// way diagnostic.ExtractDrops / evolution.PlanFromStages expect. The
// stage NEVER touches state.Trace.Stages, so Trace can be nil on the
// Recall (non-explain) hot path without breaking repair signals.
func (s *EvolutionAfterRecall) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	trace := traceFromState(state)
	err := s.runner.AfterRecall(ctx, state.Scope, trace)
	if err != nil {
		state.EvolutionErr = err
	}
	return diagnostic.EvolutionAfterRecallDetail{}, pipeline.BestEffort(err)
}

// traceFromState assembles the minimal RecallTrace the
// EvolutionRunner needs to drive repair decisions, sourcing all
// inputs from ReadState (NOT state.Trace.Stages). The synthetic
// trace places drops under FederationFanoutDetail so existing
// scanners (diagnostic.ExtractDrops, evolution.PlanFromStages) keep
// working unchanged.
func traceFromState(state *read.ReadState) domain.RecallTrace {
	drops := state.CollectMaterializeDrops()
	if len(drops) == 0 {
		return domain.RecallTrace{}
	}
	return domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "federation_fanout",
			Detail: diagnostic.FederationFanoutDetail{Drops: drops},
		}},
	}
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*EvolutionAfterRecall)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*EvolutionAfterRecall)(nil)
)
