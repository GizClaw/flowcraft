package planner

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestRuleBased_ActivatesStructuredSources(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:     domain.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		Predicate: "spouse",
		TimeRange: domain.TimeRange{From: time.Unix(1, 0)},
		Kinds:     []domain.FactKind{domain.KindEvent},
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		SourceRetrieval: true,
		SourceRelation:  true,
		SourceProfile:   true,
		SourceTimeline:  true,
	}
	for _, src := range plan.SourceOrder {
		want[src] = false
	}
	for src, left := range want {
		if left {
			t.Errorf("missing source %s in order %+v", src, plan.SourceOrder)
		}
	}
}

func TestRuleBased_StructuredBudgetsOverfetchFinalLimit(t *testing.T) {
	p := New()
	limit := 10
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:     domain.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		TimeRange: domain.TimeRange{From: time.Unix(1, 0)},
		Limit:     limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range plan.SourceOrder {
		if got := plan.SourceBudgets[src]; got != limit*SourceOverfetchMultiplier {
			t.Fatalf("budget[%s] = %d, want %d (%+v)", src, got, limit*SourceOverfetchMultiplier, plan.SourceBudgets)
		}
	}
	if plan.TotalCap != limit {
		t.Errorf("total cap = %d, want %d", plan.TotalCap, limit)
	}
}

func TestRuleBased_LimitLessThanSourcesStillOverfetchesEachSource(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:     domain.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		TimeRange: domain.TimeRange{From: time.Unix(1, 0)},
		Limit:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range plan.SourceOrder {
		if got := plan.SourceBudgets[src]; got != 4 {
			t.Fatalf("budget[%s] = %d, want 4 (%+v)", src, got, plan.SourceBudgets)
		}
	}
	if plan.TotalCap != 2 {
		t.Errorf("total cap = %d, want 2", plan.TotalCap)
	}
}

func TestRuleBased_TinyStructuredQueryDoesNotStarveStructuredSources(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:   domain.Scope{RuntimeID: "rt"},
		Subject: "alice",
		Limit:   1,
	})
	if err != nil {
		t.Fatal(err)
	}

	structuredBudget := plan.SourceBudgets[SourceRelation] + plan.SourceBudgets[SourceProfile]
	if structuredBudget == 0 {
		t.Fatalf("structured subject query with tiny limit must budget a structured source, got order=%+v budgets=%+v", plan.SourceOrder, plan.SourceBudgets)
	}
}
