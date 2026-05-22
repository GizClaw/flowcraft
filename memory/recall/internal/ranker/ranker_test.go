package ranker_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ranker"
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

func TestDefault_Rank_PreservesShortNumericQueryTerms(t *testing.T) {
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "generic", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "generic",
				Content: "Alice usually goes running after work.",
			},
		},
		{
			Candidate: domain.Candidate{FactID: "duration", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "duration",
				Content: "Alice runs for 25 minutes after work.",
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text: "Does Alice run for 25 minutes?",
		},
		FinalCap: 2,
	})
	if len(out.Items) != 2 {
		t.Fatalf("ranked len = %d, want 2", len(out.Items))
	}
	if out.Items[0].Fact.ID != "duration" {
		t.Fatalf("top ranked fact = %s, want duration", out.Items[0].Fact.ID)
	}
}

func TestDefault_Rank_NumericIntentPrefersNumberedFact(t *testing.T) {
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "no-number", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "no-number",
				Content: "Alice has cats at home.",
			},
		},
		{
			Candidate: domain.Candidate{FactID: "numbered", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "numbered",
				Content: "Alice has 2 cats at home.",
			},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text: "How many cats does Alice have?",
		},
		FinalCap: 2,
	})
	if len(out.Items) != 2 {
		t.Fatalf("ranked len = %d, want 2", len(out.Items))
	}
	if out.Items[0].Fact.ID != "numbered" {
		t.Fatalf("top ranked fact = %s, want numbered", out.Items[0].Fact.ID)
	}
}

func TestDefault_Rank_SelectedEvidenceCoverageBoost(t *testing.T) {
	r := ranker.NewDefault()
	items := []domain.ContextItem{
		{
			Candidate: domain.Candidate{FactID: "distractor", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "distractor",
				Content: "Dave took a memorable photo.",
			},
			Evidence: []domain.EvidenceRef{{ID: "ev1", Text: "Dave took the photo near a Boston gala."}},
		},
		{
			Candidate: domain.Candidate{FactID: "waterfall", Score: 0.01},
			Fact: domain.TemporalFact{
				ID:      "waterfall",
				Content: "Dave took a memorable photo.",
			},
			Evidence: []domain.EvidenceRef{{ID: "ev2", Text: "Dave took the waterfall photo in the White Mountains of New Hampshire."}},
		},
	}
	out := r.Rank(context.Background(), port.RankInput{
		Items: items,
		Intent: domain.QueryIntent{
			Text: "Where did Dave take a waterfall photo?",
		},
		FinalCap: 2,
	})
	if len(out.Items) != 2 {
		t.Fatalf("ranked len = %d, want 2", len(out.Items))
	}
	if out.Items[0].Fact.ID != "waterfall" {
		t.Fatalf("top ranked fact = %s, want waterfall", out.Items[0].Fact.ID)
	}
}
