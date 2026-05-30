package feedback

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Runner drives the feedback pipeline. Memory.Reinforce /
// Memory.Penalize construct State and call Run; the runner walks the
// stage list and emits diagnostics through the shared TelemetryHook.
type Runner struct {
	pipeline *pipeline.Pipeline[*State]
}

// NewRunner constructs a feedback Runner with the supplied stages and telemetry
// hook. The slice shape preserves drop-in room for future pre/post stages (e.g.
// quota check, audit emit) without reshaping the runner.
func NewRunner(stages []pipeline.Stage[*State], hook port.TelemetryHook) *Runner {
	return &Runner{
		pipeline: pipeline.NewPipeline(
			diagnostic.PhaseWrite,
			stages,
			hook,
			func(s *State, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
		),
	}
}

// Run executes the feedback pipeline against state.
func (r *Runner) Run(ctx context.Context, state *State) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
