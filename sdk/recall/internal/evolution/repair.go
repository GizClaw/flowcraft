package evolution

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

// RepairPlan lists fact ids operators may pass to ProjectionRebuilder
// repair APIs. Phase 8 never applies the plan automatically.
type RepairPlan struct {
	Scope   model.Scope
	FactIDs []string
	Reason  string
}

// PlanFromRecallTrace derives a repair plan from read-path drops.
// Only stale/superseded projection drift is included.
func PlanFromRecallTrace(scope model.Scope, trace model.RecallTrace) RepairPlan {
	seen := make(map[string]struct{})
	var ids []string
	for _, d := range trace.Drops {
		if d.FactID == "" {
			continue
		}
		switch d.Reason {
		case model.DropStaleFact, model.DropSuperseded:
		default:
			continue
		}
		if _, dup := seen[d.FactID]; dup {
			continue
		}
		seen[d.FactID] = struct{}{}
		ids = append(ids, d.FactID)
	}
	return RepairPlan{Scope: scope, FactIDs: ids, Reason: "recall_trace_drift"}
}

// PlanFromDrifts derives a repair plan from telemetry drift events.
func PlanFromDrifts(scope model.Scope, drifts []telemetry.DriftEvent) RepairPlan {
	seen := make(map[string]struct{})
	var ids []string
	for _, d := range drifts {
		if !driftMatchesScope(scope, d.Scope) {
			continue
		}
		if d.FactID == "" {
			continue
		}
		if d.Reason != telemetry.DriftStaleFact && d.Reason != telemetry.DriftSupersededFact {
			continue
		}
		if _, dup := seen[d.FactID]; dup {
			continue
		}
		seen[d.FactID] = struct{}{}
		ids = append(ids, d.FactID)
	}
	return RepairPlan{Scope: scope, FactIDs: ids, Reason: "telemetry_drift"}
}

func driftMatchesScope(planScope, driftScope model.Scope) bool {
	if driftScope == (model.Scope{}) {
		return true
	}
	return driftScope == planScope
}

// AttributionsFromTrace is a diagnostics convenience wrapper.
func AttributionsFromTrace(trace model.RecallTrace) []telemetry.Attribution {
	return telemetry.AttributeRecallTrace(trace)
}
