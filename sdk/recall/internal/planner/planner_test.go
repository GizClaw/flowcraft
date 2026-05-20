package planner

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestRuleBased_RetrievalOnlyWithoutEntities(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Text:  "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SourceOrder) != 1 || plan.SourceOrder[0] != SourceRetrieval {
		t.Errorf("source order = %+v, want [retrieval]", plan.SourceOrder)
	}
	if plan.Intent.Limit != DefaultLimit {
		t.Errorf("limit defaulting = %d, want %d", plan.Intent.Limit, DefaultLimit)
	}
	if plan.TotalCap != DefaultLimit {
		t.Errorf("total cap = %d, want %d", plan.TotalCap, DefaultLimit)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != DefaultLimit*SourceOverfetchMultiplier {
		t.Errorf("retrieval budget = %d, want overfetch budget %d", got, DefaultLimit*SourceOverfetchMultiplier)
	}
}

func TestRuleBased_EntityActivatedByHints(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Entities: []string{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SourceOrder) != 2 {
		t.Fatalf("source order = %+v", plan.SourceOrder)
	}
	if plan.SourceOrder[0] != SourceRetrieval || plan.SourceOrder[1] != SourceEntity {
		t.Errorf("source order = %+v", plan.SourceOrder)
	}
	if plan.SourceBudgets[SourceEntity] <= 0 {
		t.Errorf("entity budget must be > 0")
	}
}

func TestRuleBased_SourceBudgetsOverfetchFinalLimit(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:    domain.Scope{RuntimeID: "rt"},
		Entities: []string{"alice"},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != 20 {
		t.Errorf("retrieval budget = %d, want 20", got)
	}
	if got := plan.SourceBudgets[SourceEntity]; got != 20 {
		t.Errorf("entity budget = %d, want 20", got)
	}
	if plan.TotalCap != 10 {
		t.Errorf("total cap = %d, want 10", plan.TotalCap)
	}
}

func TestRuleBased_SourceBudgetCapsAtMaxOverfetch(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Limit: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.SourceBudgets[SourceRetrieval]; got != MaxSourceOverfetch {
		t.Errorf("retrieval budget = %d, want %d", got, MaxSourceOverfetch)
	}
	if plan.TotalCap != 30 {
		t.Errorf("total cap = %d, want 30", plan.TotalCap)
	}
}

func TestRuleBased_ClampsMaxLimit(t *testing.T) {
	p := New()
	plan, _ := p.Plan(context.Background(), port.PlannerInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Limit: MaxLimit + 50,
	})
	if plan.Intent.Limit != MaxLimit {
		t.Errorf("limit = %d, want clamped to %d", plan.Intent.Limit, MaxLimit)
	}
	if plan.TotalCap != MaxLimit {
		t.Errorf("total cap = %d", plan.TotalCap)
	}
}
