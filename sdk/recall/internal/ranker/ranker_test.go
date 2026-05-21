package ranker_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ranker"
)

func TestDefault_Rank_UsesEvidenceAndIntentSignals(t *testing.T) {
	now := time.Unix(1, 0)
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "generic", Score: 0.05},
			Fact: domain.TemporalFact{
				ID:         "generic",
				Kind:       domain.KindNote,
				Content:    "Caroline joined a group",
				Confidence: 0.5,
			},
		},
		{
			Candidate: domain.Candidate{FactID: "grounded", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:           "grounded",
				Kind:         domain.KindEvent,
				Subject:      "caroline",
				Entities:     []string{"caroline"},
				Content:      "Caroline went to the support group",
				ObservedAt:   now,
				Confidence:   0.8,
				EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group downtown.",
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text:     "When did Caroline go to the LGBTQ support group?",
			Entities: []string{"caroline", "lgbtq"},
			Subject:  "caroline",
			Kinds:    []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan},
			Limit:    1,
		},
		FinalCap: 1,
		Now:      now,
	})
	if len(out.Items) != 1 {
		t.Fatalf("ranked len = %d, want 1", len(out.Items))
	}
	if out.Items[0].Fact.ID != "grounded" {
		t.Fatalf("top ranked fact = %s, want grounded", out.Items[0].Fact.ID)
	}
}

func TestDefault_Rank_WentGoStemLemmaRegression(t *testing.T) {
	now := time.Now()
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "walk", Score: 0.5},
			Fact: domain.TemporalFact{
				ID:         "walk",
				Content:    "Alice walked to the store yesterday",
				ObservedAt: now,
			},
		},
		{
			Candidate: domain.Candidate{FactID: "go", Score: 0.5},
			Fact: domain.TemporalFact{
				ID:         "go",
				Content:    "Alice went to the store last week",
				ObservedAt: now,
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text:  "when did Alice go to the store",
			Limit: 2,
		},
		FinalCap: 2,
		Now:      now,
	})
	if len(out.Items) < 2 {
		t.Fatalf("want 2 items, got %d", len(out.Items))
	}
	if out.BoostsApplied == 0 {
		t.Fatal("expected rank boost when query go matches fact went via lemma+stem")
	}
}

func TestDefault_Rank_TimeDecayPrefersRecent(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-400 * 24 * time.Hour)
	recent := now.Add(-2 * 24 * time.Hour)
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "old", Score: 0.9},
			Fact: domain.TemporalFact{
				ID:         "old",
				Content:    "deployed service alpha to production",
				ObservedAt: old,
			},
		},
		{
			Candidate: domain.Candidate{FactID: "new", Score: 0.9},
			Fact: domain.TemporalFact{
				ID:         "new",
				Content:    "deployed service alpha to production",
				ObservedAt: recent,
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text:  "deploy alpha",
			Limit: 2,
		},
		FinalCap: 2,
		Now:      now,
	})
	if len(out.Items) < 2 {
		t.Fatal(out.Items)
	}
	if out.Items[0].Fact.ID != "new" {
		t.Fatalf("top = %s, want new (time decay)", out.Items[0].Fact.ID)
	}
	if out.TimeDecayApplied == 0 {
		t.Fatal("expected time decay applications")
	}
}

func TestDefault_Rank_QueryCoveragePrefersSpecificEvidence(t *testing.T) {
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "generic", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "generic",
				Kind:    domain.KindState,
				Subject: "John",
				Content: "John's mechanical engineering company failed.",
			},
		},
		{
			Candidate: domain.Candidate{FactID: "specific", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "specific",
				Kind:    domain.KindNote,
				Subject: "John",
				Content: "John is considering policymaking because of his degree and his interest in public infrastructure.",
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text: "What might John's degree be in?",
		},
		FinalCap: 2,
	})
	if len(out.Items) != 2 {
		t.Fatalf("ranked len = %d, want 2", len(out.Items))
	}
	if out.Items[0].Fact.ID != "specific" {
		t.Fatalf("top ranked fact = %s, want specific", out.Items[0].Fact.ID)
	}
}

func TestDefault_Rank_DiversifiesRepeatedEvidence(t *testing.T) {
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "a", Score: 1.00, Source: "retrieval"},
			Fact: domain.TemporalFact{
				ID:           "a",
				Kind:         domain.KindState,
				Content:      "Alice likes pottery.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "turn-1"}},
			},
		},
		{
			Candidate: domain.Candidate{FactID: "b", Score: 0.99, Source: "retrieval"},
			Fact: domain.TemporalFact{
				ID:           "b",
				Kind:         domain.KindState,
				Content:      "Alice enjoys pottery classes.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "turn-1"}},
			},
		},
		{
			Candidate: domain.Candidate{FactID: "c", Score: 0.98, Source: "entity"},
			Fact: domain.TemporalFact{
				ID:           "c",
				Kind:         domain.KindEvent,
				Content:      "Alice signed up for a pottery class yesterday.",
				EvidenceRefs: []domain.EvidenceRef{{ID: "turn-2"}},
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text: "What did Alice do for pottery?",
		},
		FinalCap: 3,
	})
	if len(out.Items) != 3 {
		t.Fatalf("ranked len = %d, want 3", len(out.Items))
	}
	if out.Items[1].Fact.ID != "c" {
		t.Fatalf("second ranked fact = %s, want diverse evidence c", out.Items[1].Fact.ID)
	}
}
