package read

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

// legacyAdapter synthesises legacy OnPipeline events from read-path
// StageDiagnostics (plan §3.B.3 C11), mirroring the inline
// emitPipeline calls the procedural runRecall used.
type legacyAdapter struct {
	inner port.TelemetryHook
	state *ReadState
}

func newLegacyAdapter(inner port.TelemetryHook, state *ReadState) *legacyAdapter {
	if inner == nil {
		inner = telemetry.NopHook{}
	}
	return &legacyAdapter{inner: inner, state: state}
}

func (a *legacyAdapter) OnProjection(ev port.ProjectionEvent) { a.inner.OnProjection(ev) }
func (a *legacyAdapter) OnDrift(ev port.DriftEvent)           { a.inner.OnDrift(ev) }
func (a *legacyAdapter) OnPipeline(ev port.PipelineEvent)     { a.inner.OnPipeline(ev) }

func (a *legacyAdapter) OnStage(d diagnostic.StageDiagnostic) {
	a.inner.OnStage(d)
	a.synthesise(d)
}

func (a *legacyAdapter) synthesise(d diagnostic.StageDiagnostic) {
	if d.Phase != diagnostic.PhaseRead {
		return
	}
	switch d.Status {
	case diagnostic.StatusOK, diagnostic.StatusFailed:
	default:
		return
	}
	switch d.Stage {
	case "intent":
		n := 0
		if det, ok := d.Detail.(diagnostic.IntentDetail); ok {
			n = len(det.Entities)
		}
		a.emit(d, "query_compile", "compile", n)
	case "plan":
		n := 0
		if a.state.Plan != nil {
			n = len(a.state.Plan.SourceOrder)
		}
		a.emit(d, "planner", "plan", n)
	case "source_fanout":
		a.synthesiseSourceFanout(d)
	case "federation_fanout":
		a.synthesiseSourceFanout(d)
		a.synthesiseFusionMaterialize(d)
		if det, ok := d.Detail.(diagnostic.FederationFanoutDetail); ok && det.FastPath {
			a.emit(d, "federation_fanout", "fanout", len(det.SubScopes))
		}
	case "federation_merge":
		n := len(a.state.MergedItems)
		a.emit(d, "federation_merge", "merge", n)
	case "fuse":
		n := 0
		if sub := a.state.PrimarySubScope(); sub != nil {
			n = len(sub.Fused)
		}
		a.emit(d, "fusion", "fuse", n)
	case "materialize":
		n := len(a.state.MergedItems)
		a.emit(d, "materialize", "materialize", n)
	case "build_hits":
		n := len(a.state.Hits)
		a.emit(d, "build_hits", "build", n)
		a.emitRerank()
	case "evolution_after_recall":
		a.emitEvolution(d)
	}
}

func (a *legacyAdapter) synthesiseFusionMaterialize(d diagnostic.StageDiagnostic) {
	fused, materialized := 0, 0
	for _, sub := range a.state.SubScopeStates {
		fused += len(sub.Fused)
		materialized += len(sub.Materialized)
	}
	a.emit(d, "fusion", "fuse", fused)
	a.emit(d, "materialize", "materialize", materialized)
}

func (a *legacyAdapter) synthesiseSourceFanout(d diagnostic.StageDiagnostic) {
	if det, ok := d.Detail.(diagnostic.SourceFanoutDetail); ok {
		for _, r := range det.Results {
			a.inner.OnPipeline(port.PipelineEvent{
				Scope:   a.state.Scope,
				Stage:   "source",
				Op:      r.Lens,
				Count:   r.Candidates,
				Latency: r.Latency,
				Err:     errFromString(r.Err),
			})
		}
		return
	}
	for _, sub := range a.state.SubScopeStates {
		for _, res := range sub.SourceResults {
			a.inner.OnPipeline(port.PipelineEvent{
				Scope:   a.state.Scope,
				Stage:   "source",
				Op:      res.Source,
				Count:   len(res.Candidates),
				Latency: res.Latency,
				Err:     res.Err,
			})
		}
	}
}

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

func (a *legacyAdapter) emitRerank() {
	if a.state.RerankErr == nil && a.state.Reranked == 0 {
		return
	}
	var err error
	if a.state.RerankErr != nil {
		err = a.state.RerankErr
	}
	a.inner.OnPipeline(port.PipelineEvent{
		Scope: a.state.Scope,
		Stage: "rerank",
		Op:    "rerank",
		Count: a.state.Reranked,
		Err:   err,
	})
}

func (a *legacyAdapter) emitEvolution(d diagnostic.StageDiagnostic) {
	if a.state.EvolutionErr == nil {
		return
	}
	n := 0
	if a.state.Trace != nil {
		n = len(a.state.Trace.Drops)
	}
	a.inner.OnPipeline(port.PipelineEvent{
		Scope:   a.state.Scope,
		Stage:   "evolution",
		Op:      "after_recall",
		Count:   n,
		Latency: d.Duration,
		Err:     a.state.EvolutionErr,
	})
}

func errFromDiag(d diagnostic.StageDiagnostic) error {
	if d.Err == "" {
		return nil
	}
	return errors.New(d.Err)
}

func errFromString(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

var _ port.TelemetryHook = (*legacyAdapter)(nil)
