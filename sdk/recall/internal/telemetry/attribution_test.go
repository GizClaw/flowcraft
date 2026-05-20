package telemetry

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

func TestAttributeRecallTrace_MapsSourceErrors(t *testing.T) {
	trace := domain.RecallTrace{
		Sources: []domain.SourceTrace{{
			Source: "retrieval",
			Err:    "search failed",
		}},
		Drops: []diagnostic.CandidateDrop{{
			Stage:  "materialize",
			Reason: diagnostic.DropStaleFact,
			FactID: "f1",
			Source: "retrieval",
		}},
	}
	got := AttributeRecallTrace(trace)
	if len(got) < 2 {
		t.Fatalf("want source + drop attributions, got %+v", got)
	}
	if got[0].Stage != StageSource || got[1].Stage != StageProjection {
		t.Fatalf("stages = %+v", got)
	}
}

func TestStageFromPipeline(t *testing.T) {
	if StageFromPipeline("conflict_resolve") != StageMerge {
		t.Fatal("conflict_resolve should map to merge")
	}
	if StageFromPipeline("build_hits") != StageAnswer {
		t.Fatal("build_hits should map to answer")
	}
}
