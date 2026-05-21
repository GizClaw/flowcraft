package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/feedback"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/feedback/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// trackingProjection captures Project / Forget invocations so tests
// can assert the single-fact reproject actually fans out.
type trackingProjection struct {
	name       string
	level      port.Consistency
	projectErr error
	projected  []domain.TemporalFact
}

func (p *trackingProjection) Name() string                  { return p.name }
func (p *trackingProjection) Consistency() port.Consistency { return p.level }
func (p *trackingProjection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if p.projectErr != nil {
		return p.projectErr
	}
	p.projected = append(p.projected, facts...)
	return nil
}
func (p *trackingProjection) Forget(context.Context, domain.Scope, []string) error { return nil }
func (p *trackingProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (p *trackingProjection) ClearScope(context.Context, domain.Scope) error { return nil }

func newRunner(t *testing.T, store port.TemporalStore, projs []port.Projection) *feedback.Runner {
	t.Helper()
	fan := pipeline.NewFanout(projs, nil)
	return feedback.NewRunner([]pipeline.Stage[*feedback.State]{
		stages.NewApplyFeedback(store, fan),
	}, nil)
}

// TestApplyFeedback_HappyPath_UpdatesAndReprojects pins the
// canonical path: the stage writes the delta into the canonical
// store, refreshes the snapshot, and pushes the updated fact through
// fanout.ProjectRequired so retrieval Doc metadata
// (MetaReinforcement / MetaPenalty) stays in sync — the Cluster D
// fix.
func TestApplyFeedback_HappyPath_UpdatesAndReprojects(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporal.NewMemoryStore()
	seed := domain.TemporalFact{ID: "f-1", Scope: scope, Kind: domain.KindNote, MergeKey: "k"}
	if err := store.Append(context.Background(), []domain.TemporalFact{seed}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	proj := &trackingProjection{name: "retrieval", level: port.Required}
	runner := newRunner(t, store, []port.Projection{proj})

	state := &feedback.State{Scope: scope, FactID: "f-1", ReinforcementDelta: 2}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Updated.Reinforcement != 2 {
		t.Errorf("Updated.Reinforcement = %v, want 2", state.Updated.Reinforcement)
	}
	if len(proj.projected) != 1 || proj.projected[0].ID != "f-1" || proj.projected[0].Reinforcement != 2 {
		t.Errorf("reproject snapshot wrong: %+v", proj.projected)
	}
	if len(state.Trace.Stages) != 1 {
		t.Fatalf("trace stages = %d, want 1", len(state.Trace.Stages))
	}
	d, ok := state.Trace.Stages[0].Detail.(diagnostic.FeedbackDetail)
	if !ok {
		t.Fatalf("detail = %T, want FeedbackDetail", state.Trace.Stages[0].Detail)
	}
	if d.FactID != "f-1" || d.ReinforcementDelta != 2 {
		t.Errorf("detail mismatch: %+v", d)
	}
}

// TestApplyFeedback_ValidationRejectsEmptyDelta is the input-contract
// guard: callers that omit both deltas are rejected before any store
// write so feedback can't be silently lost.
func TestApplyFeedback_ValidationRejectsEmptyDelta(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporal.NewMemoryStore()
	_ = store.Append(context.Background(), []domain.TemporalFact{{ID: "f", Scope: scope, Kind: domain.KindNote, MergeKey: "k"}})
	runner := newRunner(t, store, nil)
	state := &feedback.State{Scope: scope, FactID: "f"}
	if err := runner.Run(context.Background(), state); err == nil {
		t.Fatal("expected validation error for empty delta")
	}
}

// TestApplyFeedback_StoreErrorAborts pins that an UpdateFeedback
// failure (e.g. fact not found) propagates as a stage failure and
// does NOT fan out to projections.
func TestApplyFeedback_StoreErrorAborts(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporal.NewMemoryStore()
	proj := &trackingProjection{name: "retrieval", level: port.Required}
	runner := newRunner(t, store, []port.Projection{proj})

	state := &feedback.State{Scope: scope, FactID: "missing", ReinforcementDelta: 1}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err == nil {
		t.Fatal("expected store error")
	}
	if len(proj.projected) != 0 {
		t.Errorf("fanout must not run after store failure: %v", proj.projected)
	}
	if len(state.Trace.Stages) != 1 || state.Trace.Stages[0].Status != diagnostic.StatusFailed {
		t.Errorf("expected failed stage diagnostic, got %+v", state.Trace.Stages)
	}
}

// TestApplyFeedback_FanoutErrorSurfacedAsFailure pins that a
// required projection failure surfaces as a stage error — the
// canonical write happened, but downstream consumers must not see a
// silent reproject failure (the framework records Status=Failed).
func TestApplyFeedback_FanoutErrorSurfacedAsFailure(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporal.NewMemoryStore()
	seed := domain.TemporalFact{ID: "f-1", Scope: scope, Kind: domain.KindNote, MergeKey: "k"}
	_ = store.Append(context.Background(), []domain.TemporalFact{seed})
	bad := &trackingProjection{name: "retrieval", level: port.Required, projectErr: errors.New("project boom")}
	runner := newRunner(t, store, []port.Projection{bad})

	state := &feedback.State{Scope: scope, FactID: "f-1", PenaltyDelta: 1}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err == nil {
		t.Fatal("expected fanout error to surface")
	}
	if state.Trace.Stages[0].Status != diagnostic.StatusFailed {
		t.Errorf("expected Status=failed, got %v", state.Trace.Stages[0].Status)
	}
}
