package planner

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

func TestRuleBased_ActivatesStructuredSources(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), Input{
		Scope:     model.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		Predicate: "spouse",
		TimeRange: model.TimeRange{From: time.Unix(1, 0)},
		Kinds:     []model.FactKind{model.KindEvent},
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

func TestRuleBased_WeightNormalizedBudgetsSumToLimit(t *testing.T) {
	p := New()
	limit := 10
	plan, err := p.Plan(context.Background(), Input{
		Scope:     model.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		TimeRange: model.TimeRange{From: time.Unix(1, 0)},
		Limit:     limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, src := range plan.SourceOrder {
		sum += plan.SourceBudgets[src]
	}
	if sum != limit {
		t.Errorf("budget sum = %d, want %d (%+v)", sum, limit, plan.SourceBudgets)
	}
}

func TestRuleBased_LimitLessThanSourcesGivesOneEach(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), Input{
		Scope:     model.Scope{RuntimeID: "rt"},
		Subject:   "alice",
		TimeRange: model.TimeRange{From: time.Unix(1, 0)},
		Limit:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, b := range plan.SourceBudgets {
		if b > 0 {
			active++
		}
	}
	if active != 2 {
		t.Errorf("want 2 sources with budget 1, got %+v", plan.SourceBudgets)
	}
}

func TestRuleBased_TinyStructuredQueryDoesNotStarveStructuredSources(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), Input{
		Scope:   model.Scope{RuntimeID: "rt"},
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
