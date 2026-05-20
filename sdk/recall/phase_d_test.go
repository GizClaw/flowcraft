package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func TestSave_TierCoreBoostsConfidence(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	ex, ok := mem.(SaveExplainer)
	if !ok {
		t.Fatal("memory does not implement SaveExplainer")
	}
	_, traceGeneral, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier general"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, traceCore, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Tier:  domain.TierCore,
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(traceGeneral.CompiledFacts) != 1 || len(traceCore.CompiledFacts) != 1 {
		t.Fatalf("compiled facts general=%d core=%d", len(traceGeneral.CompiledFacts), len(traceCore.CompiledFacts))
	}
	if traceCore.CompiledFacts[0].Confidence <= traceGeneral.CompiledFacts[0].Confidence {
		t.Fatalf("core confidence %v want > general %v",
			traceCore.CompiledFacts[0].Confidence, traceGeneral.CompiledFacts[0].Confidence)
	}
}

func TestFork_KeepsPriorActive(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "location",
			Object:    "paris",
			Content:   "alice in paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	priorID := first.FactIDs[0]
	if _, err = mem.Fork(context.Background(), scope, priorID, TemporalFact{
		Kind:      FactState,
		Subject:   "alice",
		Predicate: "location",
		Object:    "lyon",
		Content:   "alice in lyon",
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "alice location", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var seenPrior, seenFork bool
	for _, h := range hits {
		if h.Fact.ID == priorID {
			seenPrior = true
		}
		if h.Fact.Object == "lyon" {
			seenFork = true
		}
	}
	if !seenPrior || !seenFork {
		t.Fatalf("fork recall: prior=%v fork=%v hits=%d", seenPrior, seenFork, len(hits))
	}
}

func TestTrustFilter_RemovesSecretFacts(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:     FactNote,
			Content:  "secret plan",
			Metadata: map[string]any{domain.MetaSensitivity: "secret"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:     FactNote,
			Content:  "public note",
			Metadata: map[string]any{domain.MetaSensitivity: "public"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{
		Text:  "plan note",
		Limit: 10,
		Trust: &TrustContext{MaxSensitivity: "internal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if lab, _ := h.Fact.Metadata[domain.MetaSensitivity].(string); lab == "secret" {
			t.Fatalf("secret fact leaked: %+v", h.Fact)
		}
	}
}

func TestReinforce_BoostsRank(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	low, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha beta gamma", Confidence: 0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Tier:  domain.TierCore,
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha beta gamma other", Confidence: 0.5}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.Reinforce(context.Background(), scope, low.FactIDs[0], 2.0); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "alpha beta gamma", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Fact.ID != low.FactIDs[0] {
		t.Fatalf("top hit = %s want reinforced %s", hits[0].Fact.ID, low.FactIDs[0])
	}
}
