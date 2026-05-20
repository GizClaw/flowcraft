package write

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

// legacyAdapter wraps a port.TelemetryHook and synthesises legacy
// OnPipeline events from incoming StageDiagnostics — the dual-rail
// bridge required by plan §3.B.2 C13. The adapter pinned to one
// WriteState per pipeline run so it can read fact counts that
// previously rode on the inline legacy emit-site (e.g.
// len(resolution.Facts) on the "store/append" event).
//
// OnProjection / OnDrift / OnPipeline are pass-throughs:
//
//   - OnProjection: stage internals (mostly compensators and the
//     evidence_mirror non-fatal branch) still emit legacy
//     OnProjection events directly through this adapter; they
//     forward to the user hook unchanged.
//   - OnDrift: nothing in the write path currently emits one, but
//     passing through keeps the contract symmetric with read.
//   - OnPipeline: stages never call this themselves (verified by a
//     runner-level assertion test); the field exists so the inner
//     hook's legacy contract stays intact for foreign emitters.
type legacyAdapter struct {
	inner port.TelemetryHook
	state *WriteState
}

// newLegacyAdapter builds the dual-rail wrapper. A nil inner hook
// is normalised to telemetry.NopHook so the adapter can always emit
// safely; a nil state would crash on count lookups so the caller
// MUST supply one (runner.Run does so per-call).
func newLegacyAdapter(inner port.TelemetryHook, state *WriteState) *legacyAdapter {
	if inner == nil {
		inner = telemetry.NopHook{}
	}
	return &legacyAdapter{inner: inner, state: state}
}

// OnProjection implements port.TelemetryHook.
func (a *legacyAdapter) OnProjection(ev port.ProjectionEvent) { a.inner.OnProjection(ev) }

// OnDrift implements port.TelemetryHook.
func (a *legacyAdapter) OnDrift(ev port.DriftEvent) { a.inner.OnDrift(ev) }

// OnPipeline implements port.TelemetryHook.
func (a *legacyAdapter) OnPipeline(ev port.PipelineEvent) { a.inner.OnPipeline(ev) }

// OnStage implements port.TelemetryHook. It forwards the
// StageDiagnostic to the inner hook and then synthesises whichever
// legacy OnPipeline events the corresponding runSave block fired.
func (a *legacyAdapter) OnStage(d diagnostic.StageDiagnostic) {
	a.inner.OnStage(d)
	a.synthesise(d)
}

// synthesise maps a write-phase StageDiagnostic to one or more
// legacy port.PipelineEvent values and emits them through the inner
// hook. Skipped / Compensated / ShortCircuit statuses do not produce
// legacy events — legacy runSave only emitted on actual stage
// boundaries (ok / failed).
func (a *legacyAdapter) synthesise(d diagnostic.StageDiagnostic) {
	if d.Phase != diagnostic.PhaseWrite {
		return
	}
	switch d.Status {
	case diagnostic.StatusOK, diagnostic.StatusFailed:
		// emit per the mapping below
	default:
		// Skipped / Compensated / ShortCircuit / unknown: no
		// legacy emit. The legacy code path never produced an
		// OnPipeline event for these terminal states.
		return
	}
	switch d.Stage {
	case "ingest":
		a.emit(d, "compiler", "compile", len(a.state.Ingest.Facts))
	case "resolve":
		a.emit(d, "conflict_resolve", "resolve", len(a.state.Resolution.Facts))
	case "append":
		a.emit(d, "store", "append", len(a.state.Resolution.Facts))
	case "validity_close":
		a.emitValidityClose(d)
	case "project_required":
		a.emit(d, "projection", "project_required", len(a.state.Resolution.Facts))
	case "project_optional":
		a.emit(d, "projection", "project_optional", len(a.state.Resolution.Facts))
	case "evolution_after_save":
		a.emitEvolution(d)
	}
}

// emit is the common case: one OnPipeline per StageDiagnostic with
// Latency = stage duration and Err propagated from the framework's
// StageDiagnostic.Err string.
func (a *legacyAdapter) emit(d diagnostic.StageDiagnostic, stage, op string, count int) {
	a.inner.OnPipeline(port.PipelineEvent{
		Scope:   a.state.Scope,
		Stage:   stage,
		Op:      op,
		Count:   count,
		Latency: d.Duration,
		Err:     errFromDiag(d),
	})
}

// emitValidityClose mirrors the legacy applyValidityCloses loop:
// each ErrValidityAlreadyClosed sentinel produced a per-close
// OnPipeline("store", "validity_close_already_closed") event BEFORE
// the aggregate "validity_close" event. Benign count is recovered
// from (ClosedFacts - AppliedCloses) so this code stays a pure
// projection of state.
func (a *legacyAdapter) emitValidityClose(d diagnostic.StageDiagnostic) {
	benign := 0
	if det, ok := d.Detail.(diagnostic.ValidityCloseDetail); ok {
		benign = det.ClosedFacts - len(a.state.AppliedCloses)
		if benign < 0 {
			benign = 0
		}
	}
	for i := 0; i < benign; i++ {
		a.inner.OnPipeline(port.PipelineEvent{
			Scope:   a.state.Scope,
			Stage:   "store",
			Op:      "validity_close_already_closed",
			Count:   1,
			Latency: 0,
			Err:     temporalstore.ErrValidityAlreadyClosed,
		})
	}
	a.emit(d, "store", "validity_close", len(a.state.Resolution.Closes))
}

// emitEvolution honours legacy runEvolutionAfterSave which only
// emitted a pipeline event when AfterSave returned an error.
// Success was silent.
func (a *legacyAdapter) emitEvolution(d diagnostic.StageDiagnostic) {
	if a.state.EvolutionErr == nil {
		return
	}
	a.inner.OnPipeline(port.PipelineEvent{
		Scope:   a.state.Scope,
		Stage:   "evolution",
		Op:      "after_save",
		Count:   len(a.state.AppendedFactIDs),
		Latency: d.Duration,
		Err:     a.state.EvolutionErr,
	})
}

// errFromDiag converts the framework's StageDiagnostic.Err string
// back into an error value. We synthesise a plain error wrapper —
// dropping any underlying type identity is acceptable because the
// legacy emit was always a fresh error value too.
func errFromDiag(d diagnostic.StageDiagnostic) error {
	if d.Err == "" {
		return nil
	}
	return errors.New(d.Err)
}

var _ port.TelemetryHook = (*legacyAdapter)(nil)
