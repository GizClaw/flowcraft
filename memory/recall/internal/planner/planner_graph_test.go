package planner

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestRecallStrategyPlanner_GraphOffByDefault(t *testing.T) {
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
			t.Fatalf("graph must stay off unless planner graph is enabled, order=%+v", plan.SourceOrder)
		}
	}
}

func TestRecallStrategyPlanner_GraphOnWithBridgeSeeds(t *testing.T) {
	p := New()
	p.GraphEnabled = true
	plan, err := p.Plan(context.Background(), port.PlannerInput{
		Scope:        domain.Scope{RuntimeID: "rt"},
		Entities:     []string{"alice", "bob"},
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
