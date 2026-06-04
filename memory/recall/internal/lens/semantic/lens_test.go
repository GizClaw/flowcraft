package semantic

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

func TestProjectionKindMatchesStructuredAssertions(t *testing.T) {
	subjectOnly := domain.TemporalFact{Subject: "Mira"}
	if projectionKindMatches(planner.SourceAssertion, subjectOnly) {
		t.Fatal("assertion projection must not index subject-only facts")
	}

	triple := domain.TemporalFact{Subject: "Mira", Predicate: "visited", Object: "Paris"}
	if !projectionKindMatches(planner.SourceAssertion, triple) {
		t.Fatal("assertion projection should index complete assertion triples")
	}

	contentOnly := domain.TemporalFact{Content: "Mira did not visit Paris."}
	if projectionKindMatches(planner.SourceAssertion, contentOnly) {
		t.Fatal("assertion projection must not use content-only text as eligibility")
	}
}

func TestQuerySemanticDoesNotRequireTermOverlap(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	proj := NewProjection(planner.SourceAssertion)
	if err := proj.Project(ctx, []domain.TemporalFact{{
		ID:        "fact-dave-car",
		Scope:     scope,
		Kind:      domain.KindPreference,
		Subject:   "Dave",
		Predicate: "prefers",
		Object:    "Dodge Charger",
		Content:   "Dave prefers Dodge Charger over Subaru Forester.",
	}}); err != nil {
		t.Fatalf("Project: %v", err)
	}

	got := proj.QuerySemantic(ctx, scope, domain.QueryIntent{Text: "Which city did Mira visit?"}, 10)
	if len(got) != 1 || got[0] != "fact-dave-car" {
		t.Fatalf("semantic lens should broadly discover assertions without overlap gate, got %+v", got)
	}
}

func TestQuerySemanticDoesNotHardFilterSubjectSurface(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	proj := NewProjection(planner.SourceAssertion)
	if err := proj.Project(ctx, []domain.TemporalFact{{
		ID:        "fact-annie-paris",
		Scope:     scope,
		Kind:      domain.KindEvent,
		Subject:   "Annie",
		Predicate: "visited",
		Object:    "Paris",
		Content:   "Annie visited Paris.",
	}}); err != nil {
		t.Fatalf("Project: %v", err)
	}

	got := proj.QuerySemantic(ctx, scope, domain.QueryIntent{Text: "Where did Ann visit?", Subject: "Ann"}, 10)
	if len(got) != 1 || got[0] != "fact-annie-paris" {
		t.Fatalf("subject surface mismatch should not hard-filter discovery, got %+v", got)
	}
}

func TestSourceAgentScopedQueryHonorsBudget(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}
	proj := NewProjection(planner.SourceAssertion)
	if err := proj.Project(ctx, []domain.TemporalFact{
		{
			ID:        "fact-shared",
			Scope:     domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Kind:      domain.KindState,
			Subject:   "Alice",
			Predicate: "lives in",
			Object:    "Paris",
		},
		{
			ID:        "fact-private",
			Scope:     scope,
			Kind:      domain.KindState,
			Subject:   "Alice",
			Predicate: "works at",
			Object:    "Atelier",
		},
	}); err != nil {
		t.Fatalf("Project: %v", err)
	}

	src := NewSource(planner.SourceAssertion, proj)
	got := src.Query(ctx, domain.QueryPlan{
		Intent:        domain.QueryIntent{Scope: scope, Text: "Alice"},
		SourceBudgets: map[string]int{planner.SourceAssertion: 1},
	})
	if len(got.Candidates) != 1 {
		t.Fatalf("agent-scoped semantic source must honor budget, got %+v", got.Candidates)
	}
	if !got.Truncated {
		t.Fatal("expected semantic source to mark over-budget agent-scoped discovery as truncated")
	}
}

func TestQuerySemanticBoundedSelectionPreservesRecencyAndIDOrder(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	proj := NewProjection(planner.SourceAssertion)
	facts := []domain.TemporalFact{
		{ID: "older", Scope: scope, Kind: domain.KindState, Subject: "Alice", Predicate: "visited", Object: "Rome", ObservedAt: time.Unix(1, 0)},
		{ID: "newer-b", Scope: scope, Kind: domain.KindState, Subject: "Alice", Predicate: "visited", Object: "Berlin", ObservedAt: time.Unix(5, 0)},
		{ID: "middle", Scope: scope, Kind: domain.KindState, Subject: "Alice", Predicate: "visited", Object: "Paris", ObservedAt: time.Unix(3, 0)},
		{ID: "newer-a", Scope: scope, Kind: domain.KindState, Subject: "Alice", Predicate: "visited", Object: "Oslo", ObservedAt: time.Unix(5, 0)},
		{ID: "oldest", Scope: scope, Kind: domain.KindState, Subject: "Alice", Predicate: "visited", Object: "Lima", ObservedAt: time.Unix(0, 0)},
	}
	if err := proj.Project(ctx, facts); err != nil {
		t.Fatalf("Project: %v", err)
	}

	got := proj.QuerySemantic(ctx, scope, domain.QueryIntent{Text: "anything"}, 3)
	want := []string{"newer-a", "newer-b", "middle"}
	if len(got) != len(want) {
		t.Fatalf("QuerySemantic len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("QuerySemantic order = %+v, want %+v", got, want)
		}
	}
}
