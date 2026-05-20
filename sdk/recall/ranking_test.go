package recall

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func TestRankContextItems_UsesEvidenceAndIntentSignals(t *testing.T) {
	now := time.Unix(1, 0)
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

	got := rankContextItems(items, domain.QueryIntent{
		Text:     "When did Caroline go to the LGBTQ support group?",
		Entities: []string{"caroline", "lgbtq"},
		Subject:  "caroline",
		Kinds:    []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan},
		Limit:    1,
	}, 1)
	if len(got) != 1 {
		t.Fatalf("ranked len = %d, want 1", len(got))
	}
	if got[0].Fact.ID != "grounded" {
		t.Fatalf("top ranked fact = %s, want grounded", got[0].Fact.ID)
	}
	if got[0].Candidate.Metadata["rank_boost"] == nil {
		t.Fatalf("rank boost metadata missing: %+v", got[0].Candidate)
	}
}

func TestFusionCandidateCap_OverfetchesButPreservesLargeLimits(t *testing.T) {
	if got := fusionCandidateCap(30); got != 50 {
		t.Fatalf("fusion cap = %d, want 50", got)
	}
	if got := fusionCandidateCap(100); got != 100 {
		t.Fatalf("fusion cap = %d, want 100", got)
	}
}
