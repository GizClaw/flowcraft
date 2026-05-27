package evolution

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// RepairPlan lists fact ids operators may pass to ProjectionRebuilder repair
// APIs. The plan is never applied automatically.
type RepairPlan struct {
	Scope   domain.Scope
	FactIDs []string
	Reason  string
}

// PlanFromStages derives a repair plan from read-path stage drops.
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
