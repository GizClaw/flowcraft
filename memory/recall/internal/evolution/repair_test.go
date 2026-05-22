package evolution

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

func TestPlanFromStages_StaleAndSuperseded(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromStages(scope, []diagnostic.StageDiagnostic{{
		Stage: "federation_fanout",
		Detail: diagnostic.FederationFanoutDetail{
			Drops: []diagnostic.CandidateDrop{
				{Reason: diagnostic.DropStaleFact, FactID: "a"},
				{Reason: diagnostic.DropSuperseded, FactID: "b"},
				{Reason: diagnostic.DropTotalCap, FactID: "c"},
			},
		},
	}})
	if len(plan.FactIDs) != 2 {
		t.Fatalf("want stale+superseded only, got %+v", plan.FactIDs)
	}
}

func TestPlanFromStages_DedupesAcrossStages(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	plan := PlanFromStages(scope, []diagnostic.StageDiagnostic{{
		Stage: "federation_fanout",
		Detail: diagnostic.FederationFanoutDetail{
			Drops: []diagnostic.CandidateDrop{
				{Reason: diagnostic.DropStaleFact, FactID: "x"},
				{Reason: diagnostic.DropStaleFact, FactID: "x"},
			},
		},
	}})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "x" {
		t.Fatalf("dedupe failed: %+v", plan.FactIDs)
	}
}
