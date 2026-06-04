package stages

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

func TestCandidateAssessmentTokenOverlapAndSourceScoreDoNotRescueUnsupportedCandidate(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "I am preparing visa materials now",
			Features: recallintent.ExtractFeatures("I am preparing visa materials now"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "materials-science", Source: "retrieval", Score: 0.99},
			Fact: domain.TemporalFact{
				ID:      "materials-science",
				Content: "Avery liked the materials science course.",
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("token/source score should not rescue unsupported candidate: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.DropReasons["unsupported_candidate"] != 1 {
		t.Fatalf("drop reasons = %+v, want unsupported_candidate", got.DropReasons)
	}
	if len(state.AfterTrust) != 1 {
		t.Fatalf("policy output should remain intact, got %+v", state.AfterTrust)
	}
}

func TestCandidateAssessmentSupportedSharedTokenDistractorDoesNotPassRelevance(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "I am preparing visa materials now",
			Features: recallintent.ExtractFeatures("I am preparing visa materials now"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "materials-science", Source: "retrieval", Score: 0.99},
			Fact: domain.TemporalFact{
				ID:           "materials-science",
				Content:      "Avery liked the materials science course.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "Avery said the materials science course was fun."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("supported shared-token distractor should not pass without scorer: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.DropReasons["semantic_scorer_unavailable"] != 1 {
		t.Fatalf("drop reasons = %+v, want semantic_scorer_unavailable", got.DropReasons)
	}
}

func TestCandidateAssessmentSupportedOrdinaryTokenCoverageDoesNotPassWithoutSemanticScore(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "visa application materials",
			Features: recallintent.ExtractFeatures("visa application materials"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "visa-distractor", Source: "retrieval", Score: 0.99},
			Fact: domain.TemporalFact{
				ID:           "visa-distractor",
				Content:      "The application notes mention materials for a visa.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The application notes mention materials for a visa."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("ordinary surface token coverage should not pass without semantic scorer: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.Components[0].StructuredScore != 0 {
		t.Fatalf("ordinary token coverage must not contribute structured score: %+v", got.Components[0])
	}
	if got.DropReasons["semantic_scorer_unavailable"] != 1 {
		t.Fatalf("drop reasons = %+v, want semantic_scorer_unavailable", got.DropReasons)
	}
}

func TestCandidateAssessmentStructuredFieldTokenCoverageDoesNotScoreOrPass(t *testing.T) {
	tests := []struct {
		name string
		fact domain.TemporalFact
	}{
		{
			name: "subject_subset",
			fact: domain.TemporalFact{Subject: "visa application", Predicate: "status", Object: "draft"},
		},
		{
			name: "predicate_subset",
			fact: domain.TemporalFact{Subject: "traveler", Predicate: "visa application", Object: "draft"},
		},
		{
			name: "object_subset",
			fact: domain.TemporalFact{Subject: "traveler", Predicate: "needs", Object: "visa application"},
		},
		{
			name: "subject_full_reordered",
			fact: domain.TemporalFact{Subject: "checklist application visa", Predicate: "status", Object: "draft"},
		},
		{
			name: "predicate_full_reordered",
			fact: domain.TemporalFact{Subject: "traveler", Predicate: "checklist application visa", Object: "draft"},
		},
		{
			name: "object_full_reordered",
			fact: domain.TemporalFact{Subject: "traveler", Predicate: "needs", Object: "checklist application visa"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fact.ID = tc.name
			tc.fact.EvidenceRefs = []domain.EvidenceRef{{ID: "e1", Text: "grounded evidence"}}
			component := assessCandidate(domain.ContextItem{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: tc.name, Source: "retrieval", Score: 0.99},
				Fact:      tc.fact,
			}, domain.QueryIntent{Text: "visa application checklist"}, nil)

			if component.DropReason != "semantic_scorer_unavailable" {
				t.Fatalf("ordinary typed-field token coverage should not pass without semantic scorer: %+v", component)
			}
			if component.StructuredScore != 0 {
				t.Fatalf("ordinary typed-field token coverage must not score: %+v", component)
			}
			if component.SemanticScore != 0 {
				t.Fatalf("test requires no semantic scorer path: %+v", component)
			}
		})
	}
}

func TestCandidateAssessmentAnswerShapePredicateTokensDoNotPassWithoutSemanticScore(t *testing.T) {
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "alice-location", Source: "retrieval", Score: 0.2},
		Fact: domain.TemporalFact{
			ID:           "alice-location",
			Kind:         domain.KindState,
			Subject:      "alice",
			Predicate:    "location",
			Object:       "paris",
			Content:      "alice in paris",
			EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "alice in paris"}},
		},
	}, domain.QueryIntent{Text: "alice location"}, nil)

	if component.DropReason != "semantic_scorer_unavailable" {
		t.Fatalf("ordinary answer-shape tokens should not pass deterministic fallback: %+v", component)
	}
	if component.SemanticScore != 0 {
		t.Fatalf("test requires no semantic scorer path: %+v", component)
	}
	if component.StructuredScore != 0 {
		t.Fatalf("ordinary answer-shape tokens must not contribute structured exactness: %+v", component)
	}
}

func TestCandidateAssessmentOrdinaryTokenOverlapDoesNotIncreaseLiteralScore(t *testing.T) {
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "apple-pie", Source: "retrieval", Score: 1.0},
		Fact: domain.TemporalFact{
			ID:           "apple-pie",
			Content:      "Avery baked an apple pie.",
			EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "Avery baked an apple pie."}},
		},
	}, domain.QueryIntent{
		Text: "Apple account recovery",
		Features: domain.QueryFeatures{
			Tokens: map[string]struct{}{"apple": {}, "account": {}, "recovery": {}},
		},
	}, nil)

	if component.LiteralScore != 0 {
		t.Fatalf("ordinary query token overlap must not become literal score: %+v", component)
	}
	if component.SourcePrior != 0 {
		t.Fatalf("source prior must not be derived from discovery score, got %+v", component)
	}
}

func TestCandidateAssessmentNumericQuotedSameTokenWrongRelationDoesNotPass(t *testing.T) {
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration", Source: "retrieval", Score: 0.95},
		Fact: domain.TemporalFact{
			ID:           "zxq-calibration",
			Subject:      "ZXQ 774 capsule",
			Predicate:    "calibration_status",
			Object:       "due",
			Content:      "The ZXQ 774 capsule calibration is due.",
			EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The ZXQ 774 capsule calibration is due."}},
		},
	}, domain.QueryIntent{
		Text:      "Where is \"ZXQ 774\"?",
		Subject:   "ZXQ 774 capsule",
		Predicate: "location",
		Features: domain.QueryFeatures{
			Quoted:  map[string]struct{}{"zxq": {}, "774": {}},
			Numeric: map[string]struct{}{"774": {}},
		},
	}, nil)

	if component.DropReason != "no_query_anchor_match" {
		t.Fatalf("same literal tokens with wrong relation should not pass: %+v", component)
	}
	if component.LiteralScore == 0 {
		t.Fatalf("literal exactness should remain a signal even when relation rejects: %+v", component)
	}
}

func TestCandidateAssessmentQuotedIDStatusFactDoesNotAnswerWhereWithoutLocationSlot(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:      "Where is \"ZXQ 774\"?",
			Predicate: "location",
			Features:  recallintent.ExtractFeatures("Where is \"ZXQ 774\"?"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration", Source: "retrieval", Score: 0.95},
			Fact: domain.TemporalFact{
				ID:           "zxq-calibration",
				Subject:      "ZXQ 774 capsule",
				Predicate:    "calibration_status",
				Object:       "due",
				Content:      "The ZXQ 774 capsule calibration is due.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The ZXQ 774 capsule calibration is due."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("quoted ID status fact should not answer where without location slot: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.DropReasons["no_query_anchor_match"] != 1 {
		t.Fatalf("drop reasons = %+v, want no_query_anchor_match", got.DropReasons)
	}
}

func TestCandidateAssessmentTextOnlyWhereQuotedIDStatusFactDoesNotPass(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "Where is \"ZXQ 774\"?",
			Features: recallintent.ExtractFeatures("Where is \"ZXQ 774\"?"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration", Source: "retrieval", Score: 0.95},
			Fact: domain.TemporalFact{
				ID:           "zxq-calibration",
				Subject:      "ZXQ 774 capsule",
				Predicate:    "calibration_status",
				Object:       "due",
				Content:      "The ZXQ 774 capsule calibration is due.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The ZXQ 774 capsule calibration is due."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("text-only where quoted ID should not accept status fact: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.DropReasons["no_query_anchor_match"] != 1 {
		t.Fatalf("drop reasons = %+v, want no_query_anchor_match", got.DropReasons)
	}
}

func TestCandidateAssessmentCourtesyWhereQuotedIDStatusFactDoesNotPass(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "Do you know where \"ZXQ 774\" is?",
			Features: recallintent.ExtractFeatures("Do you know where \"ZXQ 774\" is?"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration", Source: "retrieval", Score: 0.95},
			Fact: domain.TemporalFact{
				ID:           "zxq-calibration",
				Subject:      "ZXQ 774 capsule",
				Predicate:    "calibration_status",
				Object:       "due",
				Content:      "The ZXQ 774 capsule calibration is due.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The ZXQ 774 capsule calibration is due."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 0 || got.Rejected != 1 || len(state.AssessedItems) != 0 {
		t.Fatalf("courtesy where quoted ID should not accept status fact: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.DropReasons["no_query_anchor_match"] != 1 {
		t.Fatalf("drop reasons = %+v, want no_query_anchor_match", got.DropReasons)
	}
}

func TestCandidateAssessmentCourtesyWhereQuotedIDLocationFactPasses(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "Do you know where \"ZXQ 774\" is?",
			Features: recallintent.ExtractFeatures("Do you know where \"ZXQ 774\" is?"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-location", Source: "retrieval", Score: 0.12},
			Fact: domain.TemporalFact{
				ID:           "zxq-location",
				Subject:      "ZXQ 774 capsule",
				Predicate:    "location",
				Object:       "blue box",
				Content:      "The ZXQ 774 capsule is in the blue box.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "I put the ZXQ 774 capsule in the blue box."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 1 || got.Rejected != 0 || len(state.AssessedItems) != 1 {
		t.Fatalf("courtesy where quoted ID should accept location-compatible fact: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.Components[0].DropReason != "" {
		t.Fatalf("location-compatible fact should pass: %+v", got.Components[0])
	}
}

func TestCandidateAssessmentKeywordExactRecallStillPasses(t *testing.T) {
	stage := NewCandidateAssessment()
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "Paris",
			Features: recallintent.ExtractFeatures("Paris"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "paris-note", Source: "retrieval", Score: 0.4},
			Fact: domain.TemporalFact{
				ID:           "paris-note",
				Content:      "Alice loves Paris croissants.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "Alice loves Paris croissants."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 1 || got.Rejected != 0 || len(state.AssessedItems) != 1 {
		t.Fatalf("keyword exact recall should remain accepted: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.Components[0].SemanticScore != 0 {
		t.Fatalf("test requires no semantic scorer path: %+v", got.Components[0])
	}
}

func TestCandidateAssessmentLiteralExactnessPassesWithEvidence(t *testing.T) {
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq", Source: "retrieval", Score: 0.12},
		Fact: domain.TemporalFact{
			ID:           "zxq",
			Subject:      "ZXQ 774 capsule",
			Predicate:    "location",
			Object:       "blue box",
			Content:      "The ZXQ 774 capsule is in the blue box.",
			EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "I put the ZXQ 774 capsule in the blue box."}},
		},
	}, domain.QueryIntent{
		Text:      "Where is ZXQ 774?",
		Subject:   "ZXQ 774 capsule",
		Predicate: "location",
		Features: domain.QueryFeatures{
			Quoted: map[string]struct{}{"zxq": {}, "774": {}},
		},
	}, nil)

	if component.DropReason != "" || component.LiteralScore == 0 || component.SupportScore == 0 {
		t.Fatalf("literal exactness with evidence should pass assessment: %+v", component)
	}
	if math.Abs(component.RelevanceScore-0.12) < 1e-9 {
		t.Fatalf("assessment score should not reuse source score as final relevance: %+v", component)
	}
}

func TestCandidateAssessmentExplicitLiteralDateNumberAndCodeExactness(t *testing.T) {
	observedAt := time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC)
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-774", Source: "retrieval", Score: 0.07},
		Fact: domain.TemporalFact{
			ID:         "zxq-774",
			Content:    "The ZXQ-774 capsule was placed in the blue box on 2024-05-07.",
			ObservedAt: observedAt,
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "e1",
				Text:      "On 2024-05-07 I put the ZXQ-774 capsule in the blue box.",
				Timestamp: observedAt,
			}},
		},
	}, domain.QueryIntent{
		Text: "Where was \"ZXQ-774\" on 2024-05-07?",
		Features: domain.QueryFeatures{
			Quoted:  map[string]struct{}{"zxq": {}, "774": {}},
			Numeric: map[string]struct{}{"774": {}, "2024": {}, "05": {}, "07": {}},
			Temporal: domain.QueryTemporalFeatures{
				HasExplicitDate: true,
				MatchedText:     "2024-05-07",
				TimeRange:       domain.TimeRange{From: observedAt.Add(-time.Hour), To: observedAt.Add(time.Hour)},
			},
		},
	}, nil)

	if component.DropReason != "" {
		t.Fatalf("explicit literal/date/number/code-like candidate should pass: %+v", component)
	}
	if component.LiteralScore == 0 {
		t.Fatalf("explicit literals should contribute literal score: %+v", component)
	}
	if component.StructuredScore == 0 {
		t.Fatalf("explicit date range should remain a structured constraint signal: %+v", component)
	}
	if math.Abs(component.RelevanceScore-0.07) < 1e-9 {
		t.Fatalf("final relevance must not reuse source score: %+v", component)
	}
}

func TestAssessmentRankContextPackDoNotReuseSourceScoreAsHitScore(t *testing.T) {
	const sourceScore = 0.12
	state := &read.ReadState{
		Plan: &domain.QueryPlan{
			TotalCap: 1,
			Intent: domain.QueryIntent{
				Text:      "Where is \"ZXQ 774\"?",
				Subject:   "ZXQ 774 capsule",
				Predicate: "location",
				Features: domain.QueryFeatures{
					Quoted: map[string]struct{}{"zxq": {}, "774": {}},
				},
			},
		},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq", Source: "retrieval", Score: sourceScore},
			Fact: domain.TemporalFact{
				ID:           "zxq",
				Subject:      "ZXQ 774 capsule",
				Predicate:    "location",
				Object:       "blue box",
				Content:      "The ZXQ 774 capsule is in the blue box.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "I put the ZXQ 774 capsule in the blue box."}},
			},
		}},
	}

	if _, err := NewCandidateAssessment().Run(context.Background(), state); err != nil {
		t.Fatalf("assessment Run returned error: %v", err)
	}
	if math.Abs(state.AfterTrust[0].Candidate.Score-sourceScore) > 1e-9 {
		t.Fatalf("assessment must not rewrite Candidate.Score: %+v", state.AfterTrust[0].Candidate)
	}
	if _, err := NewRank(nil, false).Run(context.Background(), state); err != nil {
		t.Fatalf("rank Run returned error: %v", err)
	}
	if _, err := NewContextPack(nil).Run(context.Background(), state); err != nil {
		t.Fatalf("context pack Run returned error: %v", err)
	}
	if len(state.Hits) != 1 {
		t.Fatalf("hits = %+v", state.Hits)
	}
	if math.Abs(state.Hits[0].Score-sourceScore) < 1e-9 {
		t.Fatalf("hit score should come from assessment/rank, not source score: %+v", state.Hits[0])
	}
	if len(state.CandidateEnvelopes) != 1 {
		t.Fatalf("candidate envelopes = %+v", state.CandidateEnvelopes)
	}
	env := state.CandidateEnvelopes[0]
	if math.Abs(env.DiscoveryScore-sourceScore) > 1e-9 {
		t.Fatalf("discovery score should remain source-local: %+v", env)
	}
	if env.Assessment.RelevanceScore == 0 || math.Abs(env.Assessment.RelevanceScore-sourceScore) < 1e-9 {
		t.Fatalf("assessment should be explicit and distinct from source score: %+v", env)
	}
	if env.RankScore != state.Hits[0].Score {
		t.Fatalf("rank score should feed final hit score: env=%+v hit=%+v", env, state.Hits[0])
	}
	if !env.PackDecision.Packed || env.PackDecision.OutputRank != 1 {
		t.Fatalf("pack decision should record final packing: %+v", env.PackDecision)
	}
}

func TestCandidateAssessmentTypedSlotLinkNeedsQueryAnchorOrSemanticScore(t *testing.T) {
	component := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "title", Source: linkExpansionSource, Score: 0.8},
		Fact: domain.TemporalFact{
			ID:      "title",
			Content: "Melanie read Becoming Nicole from Caroline's recommendation.",
		},
		Link: domain.FactLink{
			ID:       "answers-slot",
			Type:     domain.LinkAnswersSlot,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "title"},
			MergeKey: "answers_slot:seed:title",
		},
	}, domain.QueryIntent{
		Text:    "Which book was mentioned?",
		Subject: "book",
	}, nil)

	if component.DropReason != "no_query_anchor_match" || component.SupportScore == 0 || component.StructuredScore == 0 {
		t.Fatalf("typed O/A/L support should score but not pass without query anchor match: %+v", component)
	}

	anchored := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "title", Source: linkExpansionSource, Score: 0.8},
		Fact: domain.TemporalFact{
			ID:      "title",
			Subject: "book",
			Content: "Melanie read Becoming Nicole from Caroline's recommendation.",
		},
		Link: domain.FactLink{
			ID:       "answers-slot",
			Type:     domain.LinkAnswersSlot,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "title"},
			MergeKey: "answers_slot:seed:title",
		},
	}, domain.QueryIntent{
		Text:    "Which book was mentioned?",
		Subject: "book",
	}, nil)
	if anchored.DropReason != "" {
		t.Fatalf("typed slot link with a matching query anchor should pass: %+v", anchored)
	}
}

func TestCandidateAssessmentTypedSlotLinkDoesNotMakeWrongSlotQuotedLiteralPass(t *testing.T) {
	for _, linkType := range []domain.FactLinkType{domain.LinkAnswersSlot, domain.LinkResolvesTo} {
		t.Run(string(linkType), func(t *testing.T) {
			component := assessCandidate(domain.ContextItem{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration", Source: linkExpansionSource, Score: 0.8},
				Fact: domain.TemporalFact{
					ID:           "zxq-calibration",
					Subject:      "ZXQ 774 capsule",
					Predicate:    "calibration_status",
					Object:       "due",
					Content:      "The ZXQ 774 capsule calibration is due.",
					EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "The ZXQ 774 capsule calibration is due."}},
				},
				Link: domain.FactLink{
					ID:   string(linkType),
					Type: linkType,
					From: domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
					To:   domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "zxq-calibration"},
				},
			}, domain.QueryIntent{
				Text:     "Where is \"ZXQ 774\"?",
				Features: recallintent.ExtractFeatures("Where is \"ZXQ 774\"?"),
			}, nil)

			if component.DropReason != "no_query_anchor_match" {
				t.Fatalf("%s link should not make wrong-slot quoted literal pass: %+v", linkType, component)
			}
			if component.LiteralScore == 0 || component.SupportScore == 0 || component.StructuredScore == 0 {
				t.Fatalf("%s link and literal should remain scoring signals: %+v", linkType, component)
			}
		})
	}
}

func TestCandidateAssessmentTypedSupportLinksCannotBypassAnchoredQuery(t *testing.T) {
	for _, linkType := range []domain.FactLinkType{
		domain.LinkSupports,
		domain.LinkDerivedFrom,
		domain.LinkAnswersSlot,
		domain.LinkResolvesTo,
		domain.LinkSameEventAs,
	} {
		t.Run(string(linkType), func(t *testing.T) {
			component := assessCandidate(domain.ContextItem{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "calibration", Source: linkExpansionSource, Score: 0.8},
				Fact: domain.TemporalFact{
					ID:      "calibration",
					Kind:    domain.KindNote,
					Subject: "ZXQ",
					Content: "ZXQ-774 calibration capsule note.",
				},
				Link: domain.FactLink{
					ID:   string(linkType),
					Type: linkType,
					From: domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
					To:   domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "calibration"},
				},
			}, domain.QueryIntent{
				Text:    "Alice favorite drink",
				Subject: "Alice",
			}, nil)
			if component.DropReason != "no_query_anchor_match" {
				t.Fatalf("%s link should not bypass anchored query mismatch: %+v", linkType, component)
			}
			if component.SupportScore == 0 {
				t.Fatalf("%s link should still contribute support/structured signal: %+v", linkType, component)
			}
		})
	}

	supports := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "supported", Source: linkExpansionSource, Score: 0.8},
		Fact:      domain.TemporalFact{ID: "supported", Kind: domain.KindNote, Content: "A supported assertion."},
		Link:      domain.FactLink{Type: domain.LinkSupports},
	}, domain.QueryIntent{Text: "Alice favorite drink", Subject: "Alice"}, nil)
	weak := assessCandidate(domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "sibling", Source: linkExpansionSource, Score: 0.8},
		Fact:      domain.TemporalFact{ID: "sibling", Kind: domain.KindNote, Content: "A same observation sibling."},
		Link:      domain.FactLink{Type: domain.LinkSameObservation},
	}, domain.QueryIntent{Text: "Alice favorite drink", Subject: "Alice"}, nil)
	if weak.SupportScore >= supports.SupportScore {
		t.Fatalf("same_observation must remain weaker than supports: supports=%+v same_observation=%+v", supports, weak)
	}
}

func TestCandidateAssessmentStageUsesInjectedSemanticScorer(t *testing.T) {
	scorer := &fakeSemanticScorer{score: 0.42, reason: "fake_semantic_match"}
	stage := NewCandidateAssessment(WithAssessmentSemanticScorer(scorer))
	state := &read.ReadState{
		Plan: &domain.QueryPlan{Intent: domain.QueryIntent{
			Text:     "I am preparing visa materials now",
			Features: recallintent.ExtractFeatures("I am preparing visa materials now"),
		}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "visa-checklist", Source: "retrieval", Score: 0.2},
			Fact: domain.TemporalFact{
				ID:           "visa-checklist",
				Content:      "Avery needs bank statements for the visa checklist.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "e1", Text: "Avery needs bank statements for the visa checklist."}},
			},
		}},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if scorer.calls != 1 {
		t.Fatalf("semantic scorer calls = %d, want 1", scorer.calls)
	}
	got := detail.(diagnostic.CandidateAssessmentDetail)
	if got.Accepted != 1 || len(state.AssessedItems) != 1 {
		t.Fatalf("semantic scorer should allow unanchored natural-language match: detail=%+v assessed=%+v", got, state.AssessedItems)
	}
	if got.Components[0].SemanticScore != scorer.score || got.Components[0].Reason != scorer.reason {
		t.Fatalf("component did not preserve scorer output: %+v", got.Components[0])
	}
}

func TestCandidateAssessmentStageUsesInjectedAssessorAndSupportReader(t *testing.T) {
	assessor := &fakeCandidateAssessor{}
	reader := fakeSupportReader{links: []domain.FactLink{{ID: "supports", Type: domain.LinkSupports}}}
	stage := NewCandidateAssessment(
		WithAssessmentAssessor(assessor),
		WithAssessmentSupportReader(reader),
	)
	state := &read.ReadState{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u"},
		Plan:  &domain.QueryPlan{Intent: domain.QueryIntent{Text: "recall anything"}},
		AfterTrust: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "fact", Source: "retrieval", Score: 0.2},
			Fact:      domain.TemporalFact{ID: "fact", Scope: domain.Scope{RuntimeID: "rt", UserID: "u"}, Content: "Stored fact."},
		}},
	}

	_, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if assessor.calls != 1 {
		t.Fatalf("assessor calls = %d, want 1", assessor.calls)
	}
	if len(assessor.last.Links) != 1 || assessor.last.Links[0].Type != domain.LinkSupports {
		t.Fatalf("AssessmentInput links = %+v, want injected support reader link", assessor.last.Links)
	}
	if len(state.AssessedItems) != 1 {
		t.Fatalf("custom assessor acceptance was not applied: %+v", state.AssessedItems)
	}
}

type fakeSemanticScorer struct {
	score  float64
	reason string
	calls  int
}

func (s *fakeSemanticScorer) Score(_ context.Context, _ string, _ domain.ContextItem) (float64, string, error) {
	s.calls++
	return s.score, s.reason, nil
}

type fakeCandidateAssessor struct {
	calls int
	last  domain.AssessmentInput
}

func (a *fakeCandidateAssessor) Assess(_ context.Context, in domain.AssessmentInput) (domain.CandidateAssessment, error) {
	a.calls++
	a.last = in
	return domain.CandidateAssessment{
		HardConstraintPass: true,
		SupportScore:       0.5,
		RelevanceScore:     0.5,
		Confidence:         0.5,
		Reason:             "fake_assessor",
	}, nil
}

type fakeSupportReader struct {
	links []domain.FactLink
}

func (r fakeSupportReader) LinksForCandidate(_ context.Context, _ domain.Scope, _ domain.ContextItem) ([]domain.FactLink, error) {
	return append([]domain.FactLink(nil), r.links...), nil
}
