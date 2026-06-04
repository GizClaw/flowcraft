package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
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
	resGeneral, _, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier general"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resCore, _, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Tier:  domain.TierCore,
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resGeneral.FactIDs) != 1 || len(resCore.FactIDs) != 1 {
		t.Fatalf("fact ids general=%d core=%d", len(resGeneral.FactIDs), len(resCore.FactIDs))
	}
	drainSideEffectsForTest(t, mem, scope)
	confGeneral := factConfidence(mem, scope, resGeneral.FactIDs[0], "tier general")
	confCore := factConfidence(mem, scope, resCore.FactIDs[0], "tier core")
	if confCore <= confGeneral {
		t.Fatalf("core confidence %v want > general %v", confCore, confGeneral)
	}
}

func factConfidence(mem Memory, scope Scope, id, text string) float64 {
	hits, err := mem.Recall(context.Background(), scope, Query{Text: text, Kinds: []FactKind{FactNote}, Limit: 5})
	if err != nil {
		panic(err)
	}
	for _, h := range hits {
		if h.Fact.ID == id {
			return h.Fact.Confidence
		}
	}
	return 0
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
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "alpha beta gamma", Kinds: []FactKind{FactNote}, Limit: 5})
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

// TestReinforce_RoutesToFeedbackPipeline verifies Memory.Reinforce runs through
// the feedback pipeline so the call emits a single apply_feedback
// StageDiagnostic and the canonical fact's reinforcement counter advances. The
// retrieval projection sees the updated MetaReinforcement on the follow-up
// reproject.
func TestReinforce_RoutesToFeedbackPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	if err := mem.Reinforce(context.Background(), scope, res.FactIDs[0], 3); err != nil {
		t.Fatalf("Reinforce: %v", err)
	}
	if !hasStage(hook.stages, "apply_feedback") {
		t.Errorf("expected apply_feedback stage diagnostic; got %v", stageNames(hook.stages))
	}
	got, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Reinforcement != 3 {
		t.Errorf("Reinforcement = %v, want 3", got.Reinforcement)
	}
}

// TestPenalize_RoutesToFeedbackPipeline is the negative-channel
// symmetric of the Reinforce route test.
func TestPenalize_RoutesToFeedbackPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	if err := mem.Penalize(context.Background(), scope, res.FactIDs[0], 1.5); err != nil {
		t.Fatalf("Penalize: %v", err)
	}
	if !hasStage(hook.stages, "apply_feedback") {
		t.Errorf("expected apply_feedback stage; got %v", stageNames(hook.stages))
	}
	got, _ := store.Get(context.Background(), scope, res.FactIDs[0])
	if got.Penalty != 1.5 {
		t.Errorf("Penalty = %v, want 1.5", got.Penalty)
	}
}
