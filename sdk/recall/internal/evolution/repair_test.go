package evolution

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

func TestPlanFromRecallTrace_StaleAndSuperseded(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromRecallTrace(scope, domain.RecallTrace{
		Drops: []diagnostic.CandidateDrop{
			{Reason: diagnostic.DropStaleFact, FactID: "a"},
			{Reason: diagnostic.DropSuperseded, FactID: "b"},
			{Reason: diagnostic.DropTotalCap, FactID: "c"},
		},
	})
	if len(plan.FactIDs) != 2 {
		t.Fatalf("want stale+superseded only, got %+v", plan.FactIDs)
	}
}

func TestPlanFromDrifts(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromDrifts(scope, []port.DriftEvent{
		{Reason: port.DriftStaleFact, FactID: "x"},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "x" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanFromDrifts_FiltersExplicitScope(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "a1"}
	plan := PlanFromDrifts(scope, []port.DriftEvent{
		{Scope: scope, Reason: port.DriftStaleFact, FactID: "keep"},
		{Scope: domain.Scope{RuntimeID: "rt", UserID: "u2", AgentID: "a1"}, Reason: port.DriftStaleFact, FactID: "drop"},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "keep" {
		t.Fatalf("plan = %+v", plan)
	}
}
