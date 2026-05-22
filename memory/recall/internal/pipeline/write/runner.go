package write

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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
//
// Phase F.1a wraps the configured telemetry hook with a per-call
// shim that enriches every StageDiagnostic with state.AsyncRequestID
// once the episode lane has stamped it. The wrapper is a no-op for
// the sync path (state.AsyncRequestID stays empty), so existing
// callers see byte-identical events.
func (r *Runner) Run(ctx context.Context, state *WriteState) error {
	if r == nil || state == nil {
		return nil
	}
	hook := r.hook
	if hook != nil {
		hook = &asyncRequestIDHook{inner: hook, state: state}
	}
	p := pipeline.NewPipeline(
		diagnostic.PhaseWrite,
		r.stages,
		hook,
		func(s *WriteState, d diagnostic.StageDiagnostic) { s.AppendStage(d) },
	)
	return p.Run(ctx, state)
}

// asyncRequestIDHook decorates a TelemetryHook so every emitted
// StageDiagnostic carries state.AsyncRequestID. The decorator is
// stateful only in the read-only sense — it captures the in-flight
// *WriteState by pointer so the AsyncRequestID stamped by build_episode
// is visible to downstream stages' emissions on the same Run.
type asyncRequestIDHook struct {
	inner port.TelemetryHook
	state *WriteState
}

func (h *asyncRequestIDHook) OnStage(d diagnostic.StageDiagnostic) {
	if h.state != nil && d.AsyncRequestID == "" && h.state.AsyncRequestID != "" {
		d.AsyncRequestID = h.state.AsyncRequestID
	}
	if h.inner != nil {
		h.inner.OnStage(d)
	}
}
