// Package telemetry provides diagnostics-oriented failure attribution over
// recall traces and pipeline events (docs §10.3 / §10.4).
package telemetry

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// FailureStage is the diagnostics taxonomy (docs §10.4). Values are
// stable wire identifiers for dashboards and repair playbooks.
type FailureStage string

const (
	StageExtract     FailureStage = "extract"
	StageNormalize   FailureStage = "normalize"
	StageEntity      FailureStage = "entity"
	StageTime        FailureStage = "time"
	StageMerge       FailureStage = "merge"
	StageProjection  FailureStage = "projection"
	StageSource      FailureStage = "source"
	StageFusion      FailureStage = "fusion"
	StageMaterialize FailureStage = "materialize"
	StageRerank      FailureStage = "rerank"
	StageAnswer      FailureStage = "answer"
)

// Attribution records one failure observation for diagnostics.
// Phase 8 does not auto-repair; operators consume these records.
type Attribution struct {
	Stage   FailureStage
	FactID  string
	Source  string
	Reason  string
	Details string
}

// StageFromDropReason maps read-path drop reasons to failure stages.
func StageFromDropReason(reason model.DropReason) FailureStage {
	switch reason {
	case model.DropStaleFact, model.DropSuperseded:
		return StageProjection
	case model.DropMaterializeErr, model.DropScopeViolation:
		return StageMaterialize
	case model.DropDuplicate, model.DropTotalCap, model.DropPerSourceCap:
		return StageFusion
	default:
		return StageMaterialize
	}
}

// StageFromPipeline maps Save/Recall pipeline stage names to failure
// stages for high-level error attribution.
func StageFromPipeline(stage string) FailureStage {
	switch stage {
	case "compiler":
		return StageExtract
	case "query_compile":
		return StageNormalize
	case "conflict_resolve":
		return StageMerge
	case "store", "evidence":
		return StageMerge
	case "projection":
		return StageProjection
	case "planner":
		return StageNormalize
	case "source":
		return StageSource
	case "fusion":
		return StageFusion
	case "materialize":
		return StageMaterialize
	case "rerank":
		return StageRerank
	case "build_hits":
		return StageAnswer
	default:
		return StageNormalize
	}
}
