package rebuild

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the rebuild-flow pipeline driver. It owns the stage
// assembly and the StageDiagnostic emission contract; the facade
// layer (sdk/recall.Memory.RebuildAll / RebuildProjection /
// RebuildScope in Phase B.4 C4) calls Run instead of hand-rolling
// stage orchestration.
//
// The zero Runner is valid and Run on it is a successful no-op —
// the smoke test below relies on that property so a freshly
// constructed Runner can be exercised before Phase B.4 wires
// concrete stages.
type Runner struct {
	pipeline *pipeline.Pipeline[*RebuildState]
}

// NewRunner constructs a rebuild Runner with the supplied stages,
// telemetry hook, and trace appender wired through the generic
// pipeline framework. stages may be nil (smoke-test path) and
// hook may be nil.
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

// Run executes the rebuild pipeline against state. The error is
// the underlying Stage failure with the framework wrapping
// nothing. ShortCircuit is treated as success and returns nil.
func (r *Runner) Run(ctx context.Context, state *RebuildState) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
