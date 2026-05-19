package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
)

// FailureStage is the diagnostics taxonomy used for recall attribution.
type FailureStage = telemetry.FailureStage

const (
	FailureExtract     = telemetry.StageExtract
	FailureNormalize   = telemetry.StageNormalize
	FailureEntity      = telemetry.StageEntity
	FailureTime        = telemetry.StageTime
	FailureMerge       = telemetry.StageMerge
	FailureProjection  = telemetry.StageProjection
	FailureSource      = telemetry.StageSource
	FailureFusion      = telemetry.StageFusion
	FailureMaterialize = telemetry.StageMaterialize
	FailureRerank      = telemetry.StageRerank
	FailureAnswer      = telemetry.StageAnswer
)

// FailureAttribution records one pipeline failure observation. Phase 8
// does not auto-repair; use RepairPlan for operator-driven fixes.
type FailureAttribution = telemetry.Attribution

// DroppedFact carries a public write-path drop reason for attribution.
type DroppedFact struct {
	Fact   TemporalFact
	Reason string
}

// RepairPlan lists fact ids suitable for ProjectionRebuilder.RepairStale.
type RepairPlan = evolution.RepairPlan

// AttributeRecallTrace maps a RecallExplain trace to failure stages.
func AttributeRecallTrace(trace RecallTrace) []FailureAttribution {
	return telemetry.AttributeRecallTrace(trace)
}

// AttributeSaveDrops maps compiler drops from Save to failure stages.
func AttributeSaveDrops(dropped []DroppedFact) []FailureAttribution {
	if len(dropped) == 0 {
		return nil
	}
	td := make([]telemetry.DroppedFact, len(dropped))
	for i, d := range dropped {
		td[i] = telemetry.DroppedFact{Fact: d.Fact, Reason: d.Reason}
	}
	return telemetry.AttributeDroppedFacts(td)
}

// RepairPlanFromTrace derives a projection repair plan from drops.
func RepairPlanFromTrace(scope Scope, trace RecallTrace) RepairPlan {
	return evolution.PlanFromRecallTrace(scope, trace)
}

// RepairPlanFromDrifts derives a repair plan from drift telemetry.
func RepairPlanFromDrifts(scope Scope, drifts []DriftEvent) RepairPlan {
	return evolution.PlanFromDrifts(scope, drifts)
}

// StageFromPipeline maps pipeline stage names to failure stages.
func StageFromPipeline(stage string) FailureStage {
	return telemetry.StageFromPipeline(stage)
}

// StageFromDropReason maps CandidateDrop reasons to failure stages.
func StageFromDropReason(reason DropReason) FailureStage {
	return telemetry.StageFromDropReason(reason)
}
