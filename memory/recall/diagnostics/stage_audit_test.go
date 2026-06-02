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

func TestAuditRecallStages_IncludesIntentRouteAndPlan(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{
			{
				Stage:  "intent_route",
				Status: diagnostic.StatusOK,
				Detail: diagnostic.IntentRouteDetail{
					QueryLen:    42,
					Entities:    []string{"Melanie"},
					Kinds:       []string{"event"},
					Subject:     "Melanie",
					Predicate:   "bought",
					Object:      "items",
					TokenCount:  5,
					QuotedCount: 1,
					Strategy:    "set",
					Confidence:  0.8,
				},
			},
			{
				Stage:  "plan",
				Status: diagnostic.StatusOK,
				Detail: diagnostic.PlanDetail{
					TotalBudget: 30,
					TaskIntents: []string{"set_completion"},
					ActivatedLenses: []diagnostic.ActivatedLens{
						{Lens: "retrieval", Weight: 1, Budget: 30, ActivatedBy: "planner"},
						{Lens: "timeline", Weight: 0.7, Budget: 8, ActivatedBy: "planner"},
					},
				},
			},
		},
	})

	if len(audit.Stages) != 2 {
		t.Fatalf("stages = %+v", audit.Stages)
	}
	query := audit.Stages[0].Query
	if query == nil || query.Subject != "Melanie" || query.Predicate != "bought" || query.Object != "items" {
		t.Fatalf("query audit = %+v", audit.Stages[0])
	}
	if query.Strategy != "set" || query.Confidence != 0.8 {
		t.Fatalf("route audit = %+v", query)
	}
	plan := audit.Stages[1]
	if plan.TotalBudget != 30 || len(plan.ActivatedLenses) != 2 || plan.ActivatedLenses[1].Lens != "timeline" {
		t.Fatalf("plan audit = %+v", plan)
	}
}

func TestAuditRecallStages_IncludesContextPackCoverageBundles(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "context_pack",
			Status: diagnostic.StatusOK,
			Detail: diagnostic.ContextPackDetail{
				CoverageBundles: []diagnostic.CoverageBundle{{
					SeedFactID:      "f1",
					RescuedFactIDs:  []string{"f2"},
					ReplacedFactIDs: []string{"noise"},
					Reason:          "subject_predicate_set",
				}},
				Hits: &[]diagnostic.CandidateSnapshot{
					{FactID: "f1", Score: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f2", Score: 0.2, Sources: []string{"retrieval"}},
				},
			},
		}},
	})

	if len(audit.Stages) != 1 {
		t.Fatalf("stages = %+v", audit.Stages)
	}
	got := audit.Stages[0]
	if got.Stage != "context_pack" || len(got.CoverageBundles) != 1 {
		t.Fatalf("context pack audit = %+v", got)
	}
	bundle := got.CoverageBundles[0]
	if bundle.SeedFactID != "f1" || bundle.RescuedFactIDs[0] != "f2" || bundle.Reason != "subject_predicate_set" {
		t.Fatalf("coverage bundle = %+v", bundle)
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
