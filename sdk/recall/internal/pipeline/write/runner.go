package write

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the write-flow pipeline driver. It owns the stage
// assembly and the StageDiagnostic emission contract; the facade
// layer (sdk/recall.Memory.Save in Phase B.2 C11) calls Run
// instead of hand-rolling stage orchestration.
//
// The zero Runner is valid and Run on it is a successful no-op —
// the smoke test below relies on that property so a freshly
// constructed Runner can be exercised before Phase B.2 wires
// concrete stages.
type Runner struct {
	pipeline *pipeline.Pipeline[*WriteState]
}

// NewRunner constructs a write Runner with the supplied stages,
// telemetry hook, and trace appender wired through the generic
// pipeline framework. stages may be nil (smoke-test path) and
// hook may be nil (the framework checks before invoking it).
//
// The trace appender is bound to WriteState.AppendStage so the
// in-flight SaveTrace.Stages slice receives every emitted
// StageDiagnostic when the caller opted in to explain output.
func NewRunner(stages []pipeline.Stage[*WriteState], hook port.TelemetryHook) *Runner {
	return &Runner{
		pipeline: pipeline.NewPipeline(
			diagnostic.PhaseWrite,
			stages,
			hook,
			func(s *WriteState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
		),
	}
}

// Run executes the write pipeline against state. The error is the
// underlying Stage failure with the framework wrapping nothing, so
// caller-side errors.Is / errors.As keep working. ShortCircuit is
// treated as success and returns nil.
func (r *Runner) Run(ctx context.Context, state *WriteState) error {
	if r == nil || r.pipeline == nil {
		return nil
	}
	return r.pipeline.Run(ctx, state)
}
