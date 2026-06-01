package diagnostics_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

func TestAuditRecallStages_IncludesCandidateExpansion(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage: "candidate_expansion",
			Detail: diagnostic.CandidateExpansionDetail{
				InputCount:       2,
				OutputCount:      2,
				Added:            1,
				AddedFactIDs:     []string{"f3"},
				Suggested:        1,
				TaskIntents:      []string{"set_completion"},
				SuggestedByTask:  map[string]int{"set_completion": 1},
				SuggestedFactIDs: []string{"f2"},
				Items: &[]diagnostic.CandidateSnapshot{
					{FactID: "f1", Score: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f2", Score: 0.22, Sources: []string{"retrieval"}},
				},
			},
		}},
	})

	if len(audit.Stages) != 1 {
		t.Fatalf("stages = %+v", audit.Stages)
	}
	got := audit.Stages[0]
	if got.Stage != "candidate_expansion" {
		t.Fatalf("stage = %q, want candidate_expansion", got.Stage)
	}
	if got.Added != 1 || len(got.AddedFactIDs) != 1 || got.AddedFactIDs[0] != "f3" {
		t.Fatalf("added summary = added:%d ids:%+v", got.Added, got.AddedFactIDs)
	}
	if got.Suggested != 1 || len(got.SuggestedFactIDs) != 1 || got.SuggestedFactIDs[0] != "f2" {
		t.Fatalf("suggestion summary = suggested:%d ids:%+v", got.Suggested, got.SuggestedFactIDs)
	}
	if got.SuggestedByTask["set_completion"] != 1 {
		t.Fatalf("suggested by task = %+v", got.SuggestedByTask)
	}
	if len(got.Candidates) != 2 || got.Candidates[1].FactID != "f2" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
}

func TestAuditRecallStages_IncludesLinkExpansion(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "link_expansion",
			Status: diagnostic.StatusOK,
			Detail: diagnostic.LinkExpansionDetail{
				InputCount:        2,
				OutputCount:       3,
				ScannedLinks:      4,
				AddedFacts:        1,
				AddedEvidenceRefs: 2,
				AddedFactIDs:      []string{"f3"},
				Items: &[]diagnostic.CandidateSnapshot{
					{FactID: "f1", Score: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f3", Score: 0.7, Sources: []string{"link_expansion"}},
				},
			},
		}},
	})

	if len(audit.Stages) != 1 {
		t.Fatalf("stages = %+v", audit.Stages)
	}
	got := audit.Stages[0]
	if got.Stage != "link_expansion" {
		t.Fatalf("stage = %q, want link_expansion", got.Stage)
	}
	if got.ScannedLinks != 4 || got.AddedFacts != 1 || got.AddedEvidenceRefs != 2 {
		t.Fatalf("link summary = %+v", got)
	}
	if len(got.AddedFactIDs) != 1 || got.AddedFactIDs[0] != "f3" {
		t.Fatalf("added fact ids = %+v", got.AddedFactIDs)
	}
	if len(got.Candidates) != 2 || got.Candidates[1].FactID != "f3" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}

	diag := diagnostics.DiagnoseRecall(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "link_expansion",
			Status: diagnostic.StatusOK,
			Detail: diagnostic.LinkExpansionDetail{
				InputCount:        2,
				OutputCount:       3,
				ScannedLinks:      4,
				AddedFacts:        1,
				AddedEvidenceRefs: 2,
				AddedFactIDs:      []string{"f3"},
			},
		}},
	}, nil)
	if !diag.LinkExpansion.Enabled || diag.LinkExpansion.AddedFacts != 1 || diag.LinkExpansion.AddedEvidenceRefs != 2 {
		t.Fatalf("diagnose link expansion = %+v", diag.LinkExpansion)
	}
}
