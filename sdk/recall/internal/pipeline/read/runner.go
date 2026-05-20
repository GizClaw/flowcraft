package read

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the read-flow pipeline driver. The facade layer
// (sdk/recall.Memory.Recall) calls Run instead of hand-rolling
// stage orchestration.
//
// Assembly order (memory.New):
//
//	intent → plan → federation_fanout → federation_merge →
//	trust_filter → rank → build_hits → evolution_after_recall
// TODO(D.5): wrap source_fanout→materialize in federation_{fanout,merge}
type Runner struct {
	stages []pipeline.Stage[*ReadState]
	hook   port.TelemetryHook
}

// NewRunner constructs a read Runner with the supplied stages and
// telemetry hook.
func NewRunner(stages []pipeline.Stage[*ReadState], hook port.TelemetryHook) *Runner {
	return &Runner{stages: stages, hook: hook}
}

// Run executes the read pipeline against state with the dual-rail
// telemetry adapter (OnStage + legacy OnPipeline synthesis).
func (r *Runner) Run(ctx context.Context, state *ReadState) error {
	if r == nil || state == nil {
		return nil
	}
	adapter := newLegacyAdapter(r.hook, state)
	p := pipeline.NewPipeline(
		diagnostic.PhaseRead,
		r.stages,
		port.TelemetryHook(adapter),
		func(s *ReadState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
	)
	return p.Run(ctx, state)
}
