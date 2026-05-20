package planner

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestRuleBased_GraphOffByDefault(t *testing.T) {
	p := New()
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:        domain.Scope{RuntimeID: "rt"},
		Entities:     []string{"alice"},
		GraphEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range plan.SourceOrder {
		if src == SourceGraph {
			t.Fatalf("graph must stay off unless RuleBased.GraphEnabled, order=%+v", plan.SourceOrder)
		}
	}
}

func TestRuleBased_GraphOnWithSeeds(t *testing.T) {
	p := New()
	p.GraphEnabled = true
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:        domain.Scope{RuntimeID: "rt"},
		Entities:     []string{"alice"},
		GraphEnabled: true,
		Limit:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceBudgets[SourceGraph] <= 0 {
		t.Fatalf("graph budget must be > 0, budgets=%+v", plan.SourceBudgets)
	}
}
