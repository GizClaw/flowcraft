package diagnostics

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// FailureStage is the diagnostics taxonomy used for recall attribution.
type FailureStage string

const (
	FailureExtract     FailureStage = "extract"
	FailureNormalize   FailureStage = "normalize"
	FailureEntity      FailureStage = "entity"
	FailureTime        FailureStage = "time"
	FailureMerge       FailureStage = "merge"
	FailureProjection  FailureStage = "projection"
	FailureSource      FailureStage = "source"
	FailureFusion      FailureStage = "fusion"
	FailureMaterialize FailureStage = "materialize"
	FailureRerank      FailureStage = "rerank"
	FailureAnswer      FailureStage = "answer"
)

// Attribution records one pipeline failure observation.
type Attribution struct {
	Stage   FailureStage
	FactID  string
	Source  string
	Reason  string
	Details string
}

// StageFromPipeline maps pipeline stage names to failure stages.
func StageFromPipeline(stage string) FailureStage {
	switch stage {
	case "compiler", "ingest":
		return FailureExtract
	case "query_compile", "intent":
		return FailureNormalize
	case "conflict_resolve", "resolve":
		return FailureMerge
	case "store", "evidence", "append", "validity_close":
		return FailureMerge
	case "projection", "project_required", "project_optional":
		return FailureProjection
	case "planner", "plan":
		return FailureNormalize
	case "source", "federation_fanout", "source_fanout":
		return FailureSource
	case "fusion", "fuse":
		return FailureFusion
	case "materialize":
		return FailureMaterialize
	case "rerank":
		return FailureRerank
	case "build_hits":
		return FailureAnswer
	default:
		return FailureNormalize
	}
}

// StageFromDropReason maps CandidateDrop reasons to failure stages.
func StageFromDropReason(reason diagnostic.DropReason) FailureStage {
	switch reason {
	case diagnostic.DropStaleFact, diagnostic.DropSuperseded:
		return FailureProjection
	case diagnostic.DropMaterializeErr, diagnostic.DropScopeViolation, diagnostic.DropRetired:
		return FailureMaterialize
	case diagnostic.DropDuplicate, diagnostic.DropTotalCap, diagnostic.DropPerSourceCap:
		return FailureFusion
	default:
		return FailureMaterialize
	}
}

// AttributeRecallTrace maps a read-path trace to failure stages.
func AttributeRecallTrace(trace domain.RecallTrace) []Attribution {
	stages := trace.Stages
	var out []Attribution
	for _, src := range diagnostic.ExtractSources(stages) {
		if src.Err == "" {
			continue
		}
		out = append(out, Attribution{
			Stage:   FailureSource,
			Source:  src.Source,
			Reason:  "source_error",
			Details: src.Err,
		})
	}
	for _, d := range diagnostic.ExtractDrops(stages) {
		out = append(out, Attribution{
			Stage:   StageFromDropReason(d.Reason),
			FactID:  d.FactID,
			Source:  d.Source,
			Reason:  string(d.Reason),
			Details: d.Details,
		})
	}
	fused := diagnostic.ExtractFusedCandidates(stages)
	mat := diagnostic.ExtractMaterialized(stages)
	if fused > 0 && mat == 0 && len(diagnostic.ExtractDrops(stages)) == 0 {
		out = append(out, Attribution{Stage: FailureMaterialize, Reason: "zero_materialized"})
	}
	return out
}

// AttributeSaveTrace maps a write-path trace to failure stages by
// inspecting the ingest stage's per-fact drop rows.
func AttributeSaveTrace(trace domain.SaveTrace) []Attribution {
	dropped := diagnostic.ExtractSaveDropped(trace.Stages)
	if len(dropped) == 0 {
		return nil
	}
	out := make([]Attribution, 0, len(dropped))
	for _, d := range dropped {
		stage := FailureExtract
		switch {
		case len(d.Reason) >= 11 && d.Reason[:11] == "governance:":
			stage = FailureNormalize
		case len(d.Reason) >= 7 && d.Reason[:7] == "policy:":
			stage = FailureNormalize
		}
		out = append(out, Attribution{
			Stage:  stage,
			Reason: d.Reason,
		})
	}
	return out
}
