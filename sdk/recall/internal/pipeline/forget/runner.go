package forget

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the scope-wide retirement pipeline driver. The facade
// layer (Memory.ForgetAll) calls Run after assembling State.
type Runner struct {
	pipeline *pipeline.Pipeline[*State]
}

// NewRunner constructs a forget Runner with the supplied stages and
// telemetry hook. The Phase D.8 C9 wiring registers a single stage
// (forget_all); the slice shape preserves drop-in room for future
// pre/post stages (audit log emit, billing) without re-shaping the
// runner.
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

// Run executes the forget pipeline against state.
func (r *Runner) Run(ctx context.Context, state *State) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
