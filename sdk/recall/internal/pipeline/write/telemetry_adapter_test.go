package write

import (
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

type captureHook struct {
	stages      []diagnostic.StageDiagnostic
	pipelines   []port.PipelineEvent
	projections []port.ProjectionEvent
	drifts      []port.DriftEvent
}

func (h *captureHook) OnStage(d diagnostic.StageDiagnostic) {
	h.stages = append(h.stages, d)
}
func (h *captureHook) OnPipeline(ev port.PipelineEvent) {
	h.pipelines = append(h.pipelines, ev)
}
func (h *captureHook) OnProjection(ev port.ProjectionEvent) {
	h.projections = append(h.projections, ev)
}
func (h *captureHook) OnDrift(ev port.DriftEvent) {
	h.drifts = append(h.drifts, ev)
}

func TestLegacyAdapter_SynthesisesIngestPipelineEvent(t *testing.T) {
	hook := &captureHook{}
	state := &WriteState{
		Scope:  domain.Scope{RuntimeID: "rt"},
		Ingest: port.IngestResult{Facts: []domain.TemporalFact{{ID: "a"}}},
	}
	a := newLegacyAdapter(hook, state)
	a.OnStage(diagnostic.StageDiagnostic{
		Stage:    "ingest",
		Phase:    diagnostic.PhaseWrite,
		Duration: 10 * time.Millisecond,
		Status:   diagnostic.StatusOK,
	})
	if len(hook.stages) != 1 {
		t.Fatalf("OnStage forwarding lost: %d", len(hook.stages))
	}
	if len(hook.pipelines) != 1 {
		t.Fatalf("OnPipeline events = %d, want 1", len(hook.pipelines))
	}
	got := hook.pipelines[0]
	if got.Stage != "compiler" || got.Op != "compile" || got.Count != 1 || got.Latency != 10*time.Millisecond {
		t.Errorf("event = %+v", got)
	}
}

func TestLegacyAdapter_StagesNotMappedAreSilent(t *testing.T) {
	hook := &captureHook{}
	a := newLegacyAdapter(hook, &WriteState{})
	a.OnStage(diagnostic.StageDiagnostic{Stage: "validate", Phase: diagnostic.PhaseWrite, Status: diagnostic.StatusOK})
	if len(hook.pipelines) != 0 {
		t.Errorf("validate must NOT synthesise OnPipeline: %+v", hook.pipelines)
	}
}

func TestLegacyAdapter_SkippedAndCompensatedSuppressOnPipeline(t *testing.T) {
	hook := &captureHook{}
	a := newLegacyAdapter(hook, &WriteState{})
	for _, s := range []diagnostic.Status{
		diagnostic.StatusSkipped,
		diagnostic.StatusCompensated,
		diagnostic.StatusShortCircuit,
	} {
		a.OnStage(diagnostic.StageDiagnostic{Stage: "ingest", Phase: diagnostic.PhaseWrite, Status: s})
	}
	if len(hook.pipelines) != 0 {
		t.Errorf("non-terminal statuses must not synthesise OnPipeline: %+v", hook.pipelines)
	}
}

func TestLegacyAdapter_FailedSurfacesErr(t *testing.T) {
	hook := &captureHook{}
	state := &WriteState{
		Scope:      domain.Scope{RuntimeID: "rt"},
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}},
	}
	a := newLegacyAdapter(hook, state)
	a.OnStage(diagnostic.StageDiagnostic{
		Stage:  "append",
		Phase:  diagnostic.PhaseWrite,
		Status: diagnostic.StatusFailed,
		Err:    "store down",
	})
	if len(hook.pipelines) != 1 || hook.pipelines[0].Err == nil {
		t.Errorf("OnPipeline event missing err: %+v", hook.pipelines)
	}
}

func TestLegacyAdapter_ValidityCloseEmitsBenignThenMain(t *testing.T) {
	hook := &captureHook{}
	state := &WriteState{
		Scope:         domain.Scope{RuntimeID: "rt"},
		Resolution:    domain.Resolution{Closes: []domain.ValidityClose{{FactID: "p1"}, {FactID: "p2"}, {FactID: "p3"}}},
		AppliedCloses: []domain.ValidityClose{{FactID: "p1"}},
	}
	a := newLegacyAdapter(hook, state)
	a.OnStage(diagnostic.StageDiagnostic{
		Stage:    "validity_close",
		Phase:    diagnostic.PhaseWrite,
		Status:   diagnostic.StatusOK,
		Duration: 5 * time.Millisecond,
		Detail:   diagnostic.ValidityCloseDetail{ClosedFacts: 3}, // 1 applied + 2 benign
	})
	if len(hook.pipelines) != 3 {
		t.Fatalf("want 3 OnPipeline events (2 benign + 1 aggregate), got %d: %+v", len(hook.pipelines), hook.pipelines)
	}
	for i := 0; i < 2; i++ {
		if hook.pipelines[i].Op != "validity_close_already_closed" {
			t.Errorf("benign[%d] Op = %q", i, hook.pipelines[i].Op)
		}
		if !errors.Is(hook.pipelines[i].Err, temporalstore.ErrValidityAlreadyClosed) {
			t.Errorf("benign[%d] err = %v", i, hook.pipelines[i].Err)
		}
	}
	if hook.pipelines[2].Op != "validity_close" {
		t.Errorf("aggregate Op = %q", hook.pipelines[2].Op)
	}
}

func TestLegacyAdapter_EvolutionOnlyEmitsOnErr(t *testing.T) {
	hook := &captureHook{}
	state := &WriteState{Scope: domain.Scope{RuntimeID: "rt"}, AppendedFactIDs: []string{"a"}}
	a := newLegacyAdapter(hook, state)
	a.OnStage(diagnostic.StageDiagnostic{Stage: "evolution_after_save", Phase: diagnostic.PhaseWrite, Status: diagnostic.StatusOK})
	if len(hook.pipelines) != 0 {
		t.Errorf("evolution success must be silent on the legacy rail: %+v", hook.pipelines)
	}
	state.EvolutionErr = errors.New("evo")
	a.OnStage(diagnostic.StageDiagnostic{Stage: "evolution_after_save", Phase: diagnostic.PhaseWrite, Status: diagnostic.StatusOK})
	if len(hook.pipelines) != 1 || hook.pipelines[0].Err == nil {
		t.Errorf("evolution failure should emit one event: %+v", hook.pipelines)
	}
}

func TestLegacyAdapter_PassesThroughProjectionAndDrift(t *testing.T) {
	hook := &captureHook{}
	a := newLegacyAdapter(hook, &WriteState{})
	a.OnProjection(port.ProjectionEvent{Projection: "x"})
	a.OnDrift(port.DriftEvent{Source: "y"})
	a.OnPipeline(port.PipelineEvent{Stage: "z"})
	if len(hook.projections) != 1 || len(hook.drifts) != 1 || len(hook.pipelines) != 1 {
		t.Errorf("pass-through dropped events: proj=%d drift=%d pipe=%d", len(hook.projections), len(hook.drifts), len(hook.pipelines))
	}
}
