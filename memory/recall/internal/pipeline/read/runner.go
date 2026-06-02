package read

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Runner is the read-flow pipeline driver. The facade layer
// (sdk/recall.Memory.Recall) calls Run instead of hand-rolling
// stage orchestration.
//
// Assembly order (memory.New):
//
//	intent_route → plan → candidate_fanout →
//	candidate_merge_and_materialize → candidate_expansion →
//	policy_filter → rank → context_pack → build_grounded_hits →
//	evolution_after_recall
type Runner struct {
	stages []pipeline.Stage[*ReadState]
	hook   port.TelemetryHook
}

// NewRunner constructs a read Runner with the supplied stages and
// telemetry hook.
func NewRunner(stages []pipeline.Stage[*ReadState], hook port.TelemetryHook) *Runner {
	return &Runner{stages: stages, hook: hook}
}

// Run executes the read pipeline against state.
func (r *Runner) Run(ctx context.Context, state *ReadState) error {
	if r == nil || state == nil {
		return nil
	}
	p := pipeline.NewPipeline(
		diagnostic.PhaseRead,
		r.stages,
		r.hook,
		func(s *ReadState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
	)
	return p.Run(ctx, state)
}
