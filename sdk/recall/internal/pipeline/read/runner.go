package read

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the read-flow pipeline driver. It owns the stage
// assembly and the StageDiagnostic emission contract; the facade
// layer (sdk/recall.Memory.Recall in Phase B.3 C10) calls Run
// instead of hand-rolling stage orchestration.
//
// The zero Runner is valid and Run on it is a successful no-op —
// the smoke test below relies on that property so a freshly
// constructed Runner can be exercised before Phase B.3 wires
// concrete stages.
type Runner struct {
	pipeline *pipeline.Pipeline[*ReadState]
}

// NewRunner constructs a read Runner with the supplied stages,
// telemetry hook, and trace appender wired through the generic
// pipeline framework. stages may be nil (smoke-test path) and
// hook may be nil.
//
// The trace appender is bound to ReadState.AppendStage so the
// in-flight RecallTrace.Stages slice receives every emitted
// StageDiagnostic when the caller opted in to explain output.
func NewRunner(stages []pipeline.Stage[*ReadState], hook port.TelemetryHook) *Runner {
	return &Runner{
		pipeline: pipeline.NewPipeline(
			diagnostic.PhaseRead,
			stages,
			hook,
			func(s *ReadState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
		),
	}
}

// Run executes the read pipeline against state. The error is the
// underlying Stage failure with the framework wrapping nothing.
// ShortCircuit is treated as success and returns nil.
func (r *Runner) Run(ctx context.Context, state *ReadState) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
