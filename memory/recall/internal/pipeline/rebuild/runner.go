package rebuild

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Runner is the rebuild-flow pipeline driver. The facade layer
// (Memory.RebuildAll / RebuildProjection) calls Run.
type Runner struct {
	pipeline *pipeline.Pipeline[*RebuildState]
}

// NewRunner constructs a rebuild Runner with the supplied stages
// and telemetry hook.
func NewRunner(stages []pipeline.Stage[*RebuildState], hook port.TelemetryHook) *Runner {
	return &Runner{
		pipeline: pipeline.NewPipeline(
			diagnostic.PhaseRebuild,
			stages,
			hook,
			func(s *RebuildState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
		),
	}
}

// Run executes the rebuild pipeline against state.
func (r *Runner) Run(ctx context.Context, state *RebuildState) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
