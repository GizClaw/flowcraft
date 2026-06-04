package diagnostics_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// TestDiagnoseSave_PopulatesFactQualityFromStats guards against a
// silent diagnostic regression: per-fact FactQuality counters
// (WithContent, WithEvidence, WithValidFrom, WithConfidence, ByKind)
// were all zero because the stage-Detail refactor dropped the
// per-fact walk. Stages now precompute FactStats before emitting
// IngestDetail / ResolveDetail; this test pins that the diagnostics
// layer reads those tallies back out instead of flat-zeroing them.
func TestDiagnoseSave_PopulatesFactQualityFromStats(t *testing.T) {
	vf := time.Date(2024, 5, 7, 0, 0, 0, 0, time.UTC)
	trace := domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{
			{
				Stage: "ingest",
				Detail: diagnostic.IngestDetail{
					ExtractedFacts: 3,
					FactStats: diagnostic.FactStats{
						Total:          3,
						WithContent:    2,
						StructuredOnly: 1,
						WithEvidence:   3,
						WithValidFrom:  2,
						WithConfidence: 2,
						ByKind:         map[string]int{"event": 2, "state": 1},
					},
				},
			},
			{
				Stage: "resolve",
				Detail: diagnostic.ResolveDetail{
					Candidates: 3,
					Appended:   2,
					FactStats: diagnostic.FactStats{
						Total:          2,
						WithContent:    2,
						WithEvidence:   2,
						WithValidFrom:  2,
						WithConfidence: 2,
						ByKind:         map[string]int{"event": 2},
					},
				},
			},
		},
	}
	diag := diagnostics.DiagnoseSave(domain.SaveRequest{}, trace)
	_ = vf

	if got := diag.Compiled; got.Total != 3 || got.WithContent != 2 ||
		got.StructuredOnly != 1 || got.WithEvidence != 3 ||
		got.WithValidFrom != 2 || got.WithConfidence != 2 ||
		got.ByKind["event"] != 2 || got.ByKind["state"] != 1 {
		t.Fatalf("Compiled = %+v", got)
	}
	if got := diag.Appended; got.Total != 2 || got.WithContent != 2 ||
		got.WithEvidence != 2 || got.WithValidFrom != 2 ||
		got.WithConfidence != 2 || got.ByKind["event"] != 2 {
		t.Fatalf("Appended = %+v", got)
	}
}

// TestDiagnoseSave_FallsBackToExtractedCountWhenStatsMissing keeps
// the diagnostics layer compatible with legacy callers that emit
// IngestDetail / ResolveDetail without precomputed FactStats (third-
// party runners, fixture-based tests). When Stats are absent the
// per-field counters stay zero but Total still reflects pipeline
// throughput so coverage dashboards do not silently lose the
// denominator.
func TestDiagnoseSave_FallsBackToExtractedCountWhenStatsMissing(t *testing.T) {
	trace := domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "ingest", Detail: diagnostic.IngestDetail{ExtractedFacts: 5}},
			{Stage: "resolve", Detail: diagnostic.ResolveDetail{Appended: 4}},
		},
	}
	diag := diagnostics.DiagnoseSave(domain.SaveRequest{}, trace)
	if diag.Compiled.Total != 5 || diag.Appended.Total != 4 {
		t.Fatalf("legacy fallback failed: Compiled=%+v Appended=%+v", diag.Compiled, diag.Appended)
	}
}

func TestDiagnoseSave_ExposesProposalLifecycle(t *testing.T) {
	trace := domain.SaveTrace{Stages: []diagnostic.StageDiagnostic{{
		Stage: "ingest",
		Detail: diagnostic.IngestDetail{
			ProposalLifecycle: diagnostic.ProposalLifecycleDetail{
				ByFamily: map[string]diagnostic.ProposalFamilyLifecycle{
					"parameter_slot": {Proposed: 2, Grounded: 1, Promoted: 1, Rejected: 1},
				},
				Grounding: diagnostic.GroundingLifecycle{
					Input:    2,
					Accepted: 1,
					Rejected: 1,
					RejectReasons: map[string]int{
						"value_not_grounded": 1,
					},
				},
				Arbitration: diagnostic.ArbitrationLifecycle{Input: 1, Winners: 1},
				Promotion:   diagnostic.PromotionLifecycle{Input: 1, Accepted: 1},
				Compile:     diagnostic.CompileLifecycle{Input: 1, Compiled: 1},
			},
		},
	}}}
	diag := diagnostics.DiagnoseSave(domain.SaveRequest{}, trace)
	if got := diag.ProposalLifecycle.ByFamily["parameter_slot"]; got.Proposed != 2 || got.Promoted != 1 {
		t.Fatalf("proposal lifecycle family = %+v", got)
	}
	if diag.ProposalLifecycle.Grounding.RejectReasons["value_not_grounded"] != 1 {
		t.Fatalf("proposal lifecycle grounding = %+v", diag.ProposalLifecycle.Grounding)
	}
}
