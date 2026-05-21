package evolution

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// RepairPlan lists fact ids operators may pass to ProjectionRebuilder
// repair APIs. Phase 8 never applies the plan automatically.
type RepairPlan struct {
	Scope   domain.Scope
	FactIDs []string
	Reason  string
}

// PlanFromStages derives a repair plan from read-path stage drops.
// Phase E.3 collapsed the legacy DriftEvent channel: drift signals
// now live in trace.Stages exclusively.
func PlanFromStages(scope domain.Scope, stages []diagnostic.StageDiagnostic) RepairPlan {
	seen := make(map[string]struct{})
	var ids []string
	for _, d := range diagnostic.ExtractDrops(stages) {
		if d.FactID == "" {
			continue
		}
		switch d.Reason {
		case diagnostic.DropStaleFact, diagnostic.DropSuperseded:
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
