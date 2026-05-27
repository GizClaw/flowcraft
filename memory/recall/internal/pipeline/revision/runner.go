package revision

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Runner is the Fork / Contest pipeline driver. Memory.Fork and
// Memory.Contest construct State and call Run; the runner walks the
// stage list (lookup_source → attach_revision → revision_save) and
// emits diagnostics through the shared TelemetryHook.
type Runner struct {
	pipeline *pipeline.Pipeline[*State]
}

// NewRunner constructs a revision Runner with the supplied stages and telemetry
// hook. The slice shape keeps room for future pre/post additions (e.g. quota /
// audit emit) without reshaping the runner.
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

// Run executes the revision pipeline against state.
func (r *Runner) Run(ctx context.Context, state *State) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
