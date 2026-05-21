package stages

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// scopeRecordingSource captures the per-call plan.Intent.Scope so the
// test can assert federation_fanout overrode it per sub-scope without
// running a second planner pass.
type scopeRecordingSource struct {
	name   string
	scopes []domain.Scope
	plans  []domain.QueryPlan
}

func (s *scopeRecordingSource) Name() string { return s.name }

func (s *scopeRecordingSource) Query(_ context.Context, plan domain.QueryPlan) domain.SourceResult {
	s.scopes = append(s.scopes, plan.Intent.Scope)
	s.plans = append(s.plans, plan)
	return domain.SourceResult{Source: s.name, Latency: time.Millisecond}
}

type stubFuser struct{}

func (stubFuser) Fuse(_ context.Context, _ []domain.SourceResult, _ port.FusionOptions) ([]domain.Candidate, []diagnostic.CandidateDrop, error) {
	return nil, nil, nil
}

type stubMaterializer struct{}

func (stubMaterializer) Materialize(_ context.Context, _ []domain.Candidate) ([]domain.ContextItem, []diagnostic.CandidateDrop, error) {
	return nil, nil, nil
}

// TestFederation_UsesGlobalPlan pins Cluster G D2: federation_fanout
// no longer constructs a planner; it must consume state.Plan
// (populated upstream) and only override the per-sub-scope
// Intent.Scope so sources can build their partition filters. This
// test also doubles as a structural guard: the stage constructor's
// signature does not accept a planner anymore.
func TestFederation_UsesGlobalPlan(t *testing.T) {
	primary := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	sibling := domain.Scope{RuntimeID: "rt"}
	scope := primary
	scope.Federation = []domain.Scope{sibling}

	src := &scopeRecordingSource{name: "retrieval"}
	plan := domain.QueryPlan{
		Intent:        domain.QueryIntent{Scope: primary, Text: "hello", Limit: 10},
		SourceOrder:   []string{"retrieval"},
		SourceBudgets: map[string]int{"retrieval": 10},
		TotalCap:      10,
	}
	stage := NewFederationFanout(
		func() []port.Source { return []port.Source{src} },
		stubFuser{},
		port.FusionOptions{},
		nil,
		stubMaterializer{},
	)

	state := &read.ReadState{
		Scope:  scope,
		Query:  domain.Query{Text: "hello"},
		Intent: &domain.QueryIntent{Text: "hello", Scope: primary, Limit: 10},
		Plan:   &plan,
		Now:    time.Now(),
	}
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	if len(src.scopes) != 2 {
		t.Fatalf("source query call count = %d, want 2 (one per sub-scope)", len(src.scopes))
	}
	gotPrimary, gotSibling := false, false
	for _, sc := range src.scopes {
		switch sc.CanonicalKey() {
		case primary.CanonicalKey():
			gotPrimary = true
		case sibling.CanonicalKey():
			gotSibling = true
		}
	}
	if !gotPrimary || !gotSibling {
		t.Fatalf("expected source queried with both sub-scopes, got primary=%v sibling=%v", gotPrimary, gotSibling)
	}
	// Strategy fields (SourceOrder / TotalCap) must match the global
	// plan unchanged across every sub-scope invocation — that is the
	// D2 invariant that lets rank / fuse and source-fanout stay
	// aligned.
	for i, gp := range src.plans {
		if len(gp.SourceOrder) != 1 || gp.SourceOrder[0] != "retrieval" {
			t.Fatalf("sub-scope plan #%d SourceOrder = %+v, want [retrieval]", i, gp.SourceOrder)
		}
		if gp.TotalCap != plan.TotalCap {
			t.Fatalf("sub-scope plan #%d TotalCap = %d, want %d (global)", i, gp.TotalCap, plan.TotalCap)
		}
	}
	// Every SubScopeState.Plan should inherit the global plan's
	// strategy; only Intent.Scope differs.
	if got := len(state.SubScopeStates); got != 2 {
		t.Fatalf("SubScopeStates len = %d, want 2", got)
	}
	for i, sub := range state.SubScopeStates {
		if sub.Plan == nil {
			t.Fatalf("sub-scope %d Plan is nil", i)
		}
		if sub.Plan.TotalCap != plan.TotalCap {
			t.Fatalf("sub-scope %d Plan.TotalCap = %d, want %d", i, sub.Plan.TotalCap, plan.TotalCap)
		}
	}
}
