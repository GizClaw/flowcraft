package write

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Runner is the write-flow pipeline driver. It owns the stage list
// (assembled once at NewRunner) and the dual-rail telemetry hook
// (legacyAdapter wraps it per call to capture state.Scope + counts
// the legacy emitPipeline read inline). The facade layer
// (sdk/recall.Memory.Save) calls Run instead of hand-rolling stage
// orchestration.
//
// The zero Runner is valid and Run on it is a successful no-op so
// the smoke-test path (no stages wired) keeps working.
type Runner struct {
	stages []pipeline.Stage[*WriteState]
	hook   port.TelemetryHook
}

// NewRunner constructs a write Runner with the supplied stages and
// telemetry hook. stages may be nil (smoke-test path) and hook may
// be nil (the framework / adapter check before invoking).
//
// The trace appender is bound inside Run to WriteState.AppendStage
// so the in-flight SaveTrace.Stages slice receives every emitted
// StageDiagnostic when explain output was requested.
func NewRunner(stages []pipeline.Stage[*WriteState], hook port.TelemetryHook) *Runner {
	return &Runner{stages: stages, hook: hook}
}

// Run executes the write pipeline against state. The dual-rail
// telemetry adapter is built fresh per call so it can capture
// state.Scope + fact counts that legacy emitPipeline read at emit
// time. ShortCircuit is treated as success and returns nil; any
// other error propagates verbatim.
func (r *Runner) Run(ctx context.Context, state *WriteState) error {
	if r == nil || state == nil {
		return nil
	}
	adapter := newLegacyAdapter(r.hook, state)
	p := pipeline.NewPipeline(
		diagnostic.PhaseWrite,
		r.stages,
		port.TelemetryHook(adapter),
		func(s *WriteState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
	)
	return p.Run(ctx, state)
}
