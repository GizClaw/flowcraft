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
					{FactID: "f1", ScoreLabel: "discovery_score", DiscoveryScore: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f2", ScoreLabel: "discovery_score", DiscoveryScore: 0.22, Sources: []string{"retrieval"}},
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
					{FactID: "f1", ScoreLabel: "final_score", FinalScore: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f2", ScoreLabel: "final_score", FinalScore: 0.2, Sources: []string{"retrieval"}},
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

func TestAuditRecallStages_IncludesCandidateAssessmentDetail(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "candidate_assessment",
			Status: diagnostic.StatusOK,
			Detail: diagnostic.CandidateAssessmentDetail{
				InputCount:  2,
				Accepted:    1,
				Rejected:    1,
				OutputCount: 1,
				Dropped:     1,
				DropReasons: map[string]int{"unsupported_candidate": 1},
				ScoreSummary: diagnostic.CandidateAssessmentScoreSummary{
					Count:                2,
					RelevanceScoreMin:    0.1,
					RelevanceScoreMax:    0.8,
					RelevanceScoreAvg:    0.45,
					SemanticScoreAvg:     0.2,
					SupportScoreAvg:      0.3,
					StructuredScoreAvg:   0.1,
					LiteralScoreAvg:      0.05,
					SourcePriorAvg:       0.02,
					ConfidenceAvg:        0.7,
					HardConstraintPasses: 2,
				},
				Components: []diagnostic.CandidateAssessmentComponent{{
					ID:                 "f1",
					Kind:               "assertion",
					HardConstraintPass: true,
					SupportScore:       0.5,
					StructuredScore:    0.2,
					LiteralScore:       0.1,
					SemanticScore:      0.4,
					SourcePrior:        0.02,
					RelevanceScore:     0.8,
					Confidence:         0.7,
					Reason:             "supported",
					EquivalenceGroup:   "eq:f1",
					SupportGroup:       "obs:o1",
					DiversityGroup:     "source:retrieval",
				}, {
					ID:                 "f2",
					Kind:               "assertion",
					HardConstraintPass: true,
					DropReason:         "unsupported_candidate",
					FallbackReason:     "semantic_scorer_unavailable",
				}},
				Items: &[]diagnostic.CandidateSnapshot{{FactID: "f1", ScoreLabel: "assessment_relevance_score", AssessmentScore: 0.8, Sources: []string{"retrieval"}}},
			},
		}},
	})

	if len(audit.Stages) != 1 {
		t.Fatalf("stages = %+v", audit.Stages)
	}
	got := audit.Stages[0]
	if got.Stage != "candidate_assessment" || got.InputCount != 2 || got.OutputCount != 1 || got.Dropped != 1 {
		t.Fatalf("assessment summary = %+v", got)
	}
	if got.DropReasons["unsupported_candidate"] != 1 {
		t.Fatalf("drop reasons = %+v", got.DropReasons)
	}
	if got.ScoreSummary == nil || got.ScoreSummary.RelevanceScoreMax != 0.8 || got.ScoreSummary.HardConstraintPasses != 2 {
		t.Fatalf("score summary = %+v", got.ScoreSummary)
	}
	if len(got.Assessment) != 2 || got.Assessment[0].SemanticScore != 0.4 || got.Assessment[1].FallbackReason == "" {
		t.Fatalf("assessment components = %+v", got.Assessment)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].FactID != "f1" {
		t.Fatalf("assessment candidates = %+v", got.Candidates)
	}
}

func TestAuditRecallStages_LabelsStageSpecificScores(t *testing.T) {
	audit := diagnostics.AuditRecallStages(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{
			{
				Stage:  "candidate_fanout",
				Status: diagnostic.StatusOK,
				Detail: diagnostic.CandidateFanoutDetail{Sources: []diagnostic.SourceResult{{
					Lens: "retrieval",
					Snapshots: &[]diagnostic.CandidateSnapshot{{
						FactID:         "f1",
						Source:         "retrieval",
						Rank:           1,
						ScoreLabel:     "discovery_score",
						DiscoveryScore: 0.99,
					}},
				}}},
			},
			{
				Stage:  "rank",
				Status: diagnostic.StatusOK,
				Detail: diagnostic.RankDetail{Output: &[]diagnostic.CandidateSnapshot{{
					FactID:     "f1",
					ScoreLabel: "rank_score",
					RankScore:  0.42,
				}}},
			},
			{
				Stage:  "build_grounded_hits",
				Status: diagnostic.StatusOK,
				Detail: diagnostic.BuildGroundedHitsDetail{Hits: &[]diagnostic.CandidateSnapshot{{
					FactID:     "f1",
					ScoreLabel: "final_score",
					FinalScore: 0.42,
				}}},
			},
		},
	})

	var discovery, rank, final diagnostics.RecallCandidateSnapshot
	for _, stage := range audit.Stages {
		switch stage.Stage {
		case "candidate_fanout":
			discovery = stage.Candidates[0]
		case "rank_output":
			rank = stage.Candidates[0]
		case "build_grounded_hits":
			final = stage.Candidates[0]
		}
	}
	if discovery.ScoreLabel != "discovery_score" || discovery.DiscoveryScore != 0.99 {
		t.Fatalf("discovery snapshot = %+v", discovery)
	}
	if rank.ScoreLabel != "rank_score" || rank.RankScore != 0.42 {
		t.Fatalf("rank snapshot = %+v", rank)
	}
	if final.ScoreLabel != "final_score" || final.FinalScore != 0.42 {
		t.Fatalf("final snapshot = %+v", final)
	}
	if discovery.DiscoveryScore == final.FinalScore {
		t.Fatalf("source discovery score should remain distinct from final hit score: discovery=%+v final=%+v", discovery, final)
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
					{FactID: "f1", ScoreLabel: "discovery_score", DiscoveryScore: 0.9, Sources: []string{"retrieval"}},
					{FactID: "f3", ScoreLabel: "discovery_score", DiscoveryScore: 0.7, Sources: []string{"link_expansion"}},
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
