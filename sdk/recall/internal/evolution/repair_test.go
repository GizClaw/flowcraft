package evolution

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

func TestPlanFromRecallTrace_StaleAndSuperseded(t *testing.T) {
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromRecallTrace(scope, model.RecallTrace{
		Drops: []model.CandidateDrop{
			{Reason: model.DropStaleFact, FactID: "a"},
			{Reason: model.DropSuperseded, FactID: "b"},
			{Reason: model.DropTotalCap, FactID: "c"},
		},
	})
	if len(plan.FactIDs) != 2 {
		t.Fatalf("want stale+superseded only, got %+v", plan.FactIDs)
	}
}

func TestPlanFromDrifts(t *testing.T) {
	scope := model.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromDrifts(scope, []telemetry.DriftEvent{
		{Reason: telemetry.DriftStaleFact, FactID: "x"},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "x" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanFromDrifts_FiltersExplicitScope(t *testing.T) {
	scope := model.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "a1"}
	plan := PlanFromDrifts(scope, []telemetry.DriftEvent{
		{Scope: scope, Reason: telemetry.DriftStaleFact, FactID: "keep"},
		{Scope: model.Scope{RuntimeID: "rt", UserID: "u2", AgentID: "a1"}, Reason: telemetry.DriftStaleFact, FactID: "drop"},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "keep" {
		t.Fatalf("plan = %+v", plan)
	}
}
