package stages

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

func TestCandidateExpansionSuggestsSetCompletionSiblings(t *testing.T) {
	stage := NewCandidateExpansion(nil)
	state := &read.ReadState{
		Plan: &domain.QueryPlan{
			TotalCap:    1,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Tokens: map[string]struct{}{"alice": {}, "bought": {}, "items": {}},
			}},
		},
		MergedItems: []domain.ContextItem{
			expansionItem("figurines", 0.90, "Alice", "bought", "ceramic figurines", "D1:1", "Alice bought ceramic figurines."),
			expansionItem("shoes", 0.10, "Alice", "bought", "red shoes", "D1:2", "Alice bought red shoes."),
			expansionItem("paris", 0.80, "Alice", "likes", "Paris", "D2:1", "Alice likes Paris."),
		},
	}

	d, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	detail := d.(diagnostic.CandidateExpansionDetail)
	if detail.Suggested != 1 {
		t.Fatalf("suggested = %d, want 1", detail.Suggested)
	}
	if len(detail.SuggestedFactIDs) != 1 || detail.SuggestedFactIDs[0] != "shoes" {
		t.Fatalf("suggested fact ids = %+v, want [shoes]", detail.SuggestedFactIDs)
	}
	if state.MergedItems[1].Candidate.Score != 0.10 {
		t.Fatalf("shadow expansion must not change score, got %f", state.MergedItems[1].Candidate.Score)
	}
	if sources, ok := state.MergedItems[1].Candidate.Metadata["sources"]; ok {
		t.Fatalf("evidence expansion must not mutate source attribution, got metadata sources=%+v", sources)
	}
	if got := candidateSources(state.MergedItems[1].Candidate); len(got) != 1 || got[0] != "retrieval" {
		t.Fatalf("candidate sources = %+v, want [retrieval]", got)
	}
	if len(state.MergedItems[1].Candidate.Metadata) != 0 {
		t.Fatalf("shadow expansion must not mutate candidate metadata, got %+v", state.MergedItems[1].Candidate.Metadata)
	}
	if state.MergedItems[2].Candidate.Score != 0.80 {
		t.Fatalf("unrelated item should not be boosted, got score %f", state.MergedItems[2].Candidate.Score)
	}
}

func TestCandidateExpansionSuggestsBridgeSameEvidenceGroup(t *testing.T) {
	stage := NewCandidateExpansion(nil)
	state := &read.ReadState{
		Plan: &domain.QueryPlan{
			TotalCap:    1,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskBridgeResolution},
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Tokens: map[string]struct{}{"alice": {}, "buy": {}, "necklace": {}, "wore": {}, "where": {}, "paris": {}},
			}},
		},
		MergedItems: []domain.ContextItem{
			expansionItem("wore", 0.90, "Alice", "wore", "necklace", "D1:1", "Alice wore the necklace to dinner."),
			expansionItem("bought", 0.10, "Alice", "bought", "necklace", "D1:2", "Alice bought the necklace in Paris."),
			expansionItem("dog", 0.80, "Alice", "walked", "dog", "D2:1", "Alice walked her dog."),
		},
	}

	d, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	detail := d.(diagnostic.CandidateExpansionDetail)
	if detail.Suggested != 1 || len(detail.SuggestedFactIDs) != 1 || detail.SuggestedFactIDs[0] != "bought" {
		t.Fatalf("suggested bridge facts = %d %+v, want [bought]", detail.Suggested, detail.SuggestedFactIDs)
	}
	if state.MergedItems[1].Candidate.Score != 0.10 {
		t.Fatalf("shadow expansion must not change bridge-associated score, got %f", state.MergedItems[1].Candidate.Score)
	}
	if state.MergedItems[2].Candidate.Score != 0.80 {
		t.Fatalf("unassociated item should not be boosted, got score %f", state.MergedItems[2].Candidate.Score)
	}
}

func expansionItem(id string, score float64, subject, predicate, object, evidenceID, evidenceText string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: id, Source: "retrieval", Score: score, EvidenceIDs: []string{evidenceID}},
		Fact: domain.TemporalFact{
			ID:        id,
			Kind:      domain.KindState,
			Content:   evidenceText,
			Subject:   subject,
			Predicate: predicate,
			Object:    object,
		},
		Evidence: []domain.EvidenceRef{{ID: evidenceID, Text: evidenceText}},
	}
}
