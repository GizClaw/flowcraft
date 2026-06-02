package stages

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

func TestCandidateExpansionAddsCappedSubjectPredicateSiblings(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporalstore.NewMemoryStore()
	facts := []domain.TemporalFact{
		neighborFact(scope, "bailey", "Jordan has a cat named Bailey.", "Jordan", "has_pet", "Bailey"),
		neighborFact(scope, "oliver", "Jordan has a pet dog named Oliver.", "Jordan", "has_pet", "Oliver"),
		neighborFact(scope, "luna", "Jordan has a pet dog named Luna.", "Jordan", "has_pet", "Luna"),
		neighborFact(scope, "hiking", "Jordan went hiking.", "Jordan", "went", "hiking"),
	}
	if err := store.Append(context.Background(), facts); err != nil {
		t.Fatalf("append facts: %v", err)
	}
	query := "What pets does Jordan have?"
	stage := NewCandidateExpansion(store)
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: query},
		Plan: &domain.QueryPlan{
			Intent: domain.QueryIntent{
				Text:     query,
				Entities: []string{"Jordan"},
				Features: recallintent.ExtractFeatures(query),
			},
			TotalCap:    12,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
		},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "bailey", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
			Evidence:  facts[0].EvidenceRefs,
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, item := range state.MergedItems {
		got[item.Fact.ID] = true
	}
	for _, id := range []string{"bailey", "oliver", "luna"} {
		if !got[id] {
			t.Fatalf("neighbor candidate recall should add sibling %q, got %+v", id, state.MergedItems)
		}
	}
	if got["hiking"] {
		t.Fatalf("unrelated same-entity fact should not be added as a sibling: %+v", state.MergedItems)
	}
}

func TestCandidateExpansionUsesSeedFactAnchorsForStructuralSiblings(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporalstore.NewMemoryStore()
	facts := []domain.TemporalFact{
		neighborFact(scope, "bailey", "Jordan has a cat named Bailey.", "Jordan", "has_pet", "Bailey"),
		neighborFact(scope, "oliver", "Jordan has a pet dog named Oliver.", "Jordan", "has_pet", "Oliver"),
	}
	if err := store.Append(context.Background(), facts); err != nil {
		t.Fatalf("append facts: %v", err)
	}
	query := "What pets are there?"
	stage := NewCandidateExpansion(store)
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: query},
		Plan: &domain.QueryPlan{
			Intent: domain.QueryIntent{
				Text:     query,
				Features: recallintent.ExtractFeatures(query),
			},
			TotalCap:    12,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
		},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "bailey", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
			Evidence:  facts[0].EvidenceRefs,
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, item := range state.MergedItems {
		got[item.Fact.ID] = true
	}
	if !got["oliver"] {
		t.Fatalf("seed subject anchor should add sibling pet fact, got %+v", state.MergedItems)
	}
}

func TestCandidateExpansionPropagatesCanceledContext(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	store := temporalstore.NewMemoryStore()
	query := "What pets does Jordan have?"
	stage := NewCandidateExpansion(store)
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: query},
		Plan: &domain.QueryPlan{
			Intent: domain.QueryIntent{
				Text:     query,
				Entities: []string{"Jordan"},
				Features: recallintent.ExtractFeatures(query),
			},
			TotalCap:    12,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := stage.Run(ctx, state)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("candidate expansion should propagate canceled context, got %v", err)
	}
}

func neighborFact(scope domain.Scope, id, content, subject, predicate, object string) domain.TemporalFact {
	return domain.TemporalFact{
		ID:        id,
		Scope:     scope,
		Kind:      domain.KindState,
		Content:   content,
		Subject:   subject,
		Predicate: predicate,
		Object:    object,
		Entities:  []string{"Jordan", "jordan"},
		EvidenceRefs: []domain.EvidenceRef{{
			ID:   "ev-" + id,
			Text: content,
		}},
	}
}
