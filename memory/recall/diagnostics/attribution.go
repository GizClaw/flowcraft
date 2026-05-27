package diagnostics

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// FailureStage is the diagnostics taxonomy used for recall attribution.
type FailureStage string

const (
	FailureExtract              FailureStage = "extract"
	FailureNormalize            FailureStage = "normalize"
	FailureEntity               FailureStage = "entity"
	FailureTime                 FailureStage = "time"
	FailureMerge                FailureStage = "merge"
	FailureProjection           FailureStage = "projection"
	FailureSource               FailureStage = "source"
	FailureCandidateMerge       FailureStage = "candidate_merge"
	FailureCandidateMaterialize FailureStage = "candidate_materialize"
	FailureRerank               FailureStage = "rerank"
	FailureAnswer               FailureStage = "answer"
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
	case "query_compile", "query_understand", "intent":
		return FailureNormalize
	case "conflict_resolve", "resolve":
		return FailureMerge
	case "store", "evidence", "append", "validity_close":
		return FailureMerge
	case "projection", "project_required", "project_optional":
		return FailureProjection
	case "planner", "plan":
		return FailureNormalize
	case "source", "candidate_fanout":
		return FailureSource
	case "candidate_merge", "candidate_merge_and_materialize":
		return FailureCandidateMerge
	case "candidate_materialize":
		return FailureCandidateMaterialize
	case "rerank":
		return FailureRerank
	case "context_pack", "build_grounded_hits":
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
		return FailureCandidateMaterialize
	case diagnostic.DropDuplicate, diagnostic.DropTotalCap, diagnostic.DropPerSourceCap:
		return FailureCandidateMerge
	default:
		return FailureCandidateMaterialize
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
	candidates := diagnostic.ExtractCandidateCount(stages)
	mat := diagnostic.ExtractMaterialized(stages)
	if candidates > 0 && mat == 0 && len(diagnostic.ExtractDrops(stages)) == 0 {
		out = append(out, Attribution{Stage: FailureCandidateMaterialize, Reason: "zero_materialized"})
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
