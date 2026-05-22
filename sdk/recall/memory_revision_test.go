package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

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
	drainSideEffectsForTest(t, mem, scope)
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

// TestFork_RoutesToRevisionPipeline pins that Memory.Fork emits the
// three revision pipeline stages (lookup_source / attach / save) AND
// produces a new canonical fact carrying the RevisionFork annotation
// while leaving the source fact active.
func TestFork_RoutesToRevisionPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	res, err := mem.Fork(context.Background(), scope, first.FactIDs[0], TemporalFact{
		Kind:      FactState,
		Subject:   "alice",
		Predicate: "city",
		Content:   "Lyon",
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	for _, name := range []string{"revision_lookup_source", "revision_attach", "revision_save"} {
		if !hasStage(hook.stages, name) {
			t.Errorf("expected stage %q; got %v", name, stageNames(hook.stages))
		}
	}
	created, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	rev, ok := domain.RevisionOf(created)
	if !ok || rev.Kind != domain.RevisionFork || rev.SourceFactID != first.FactIDs[0] {
		t.Errorf("Revision = %+v ok=%v, want fork/%s", rev, ok, first.FactIDs[0])
	}
	prior, _ := store.Get(context.Background(), scope, first.FactIDs[0])
	if prior.ValidTo != nil || prior.CorrectedBy != "" {
		t.Errorf("Fork must NOT close source; got ValidTo=%v CorrectedBy=%q", prior.ValidTo, prior.CorrectedBy)
	}
}

// TestContest_RoutesToRevisionPipeline pins that Memory.Contest
// emits the three revision pipeline stages and creates a FactNote
// carrying the RevisionContest annotation.
func TestContest_RoutesToRevisionPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	res, err := mem.Contest(context.Background(), scope, first.FactIDs[0], []EvidenceRef{{ID: "ev-1"}})
	if err != nil {
		t.Fatalf("Contest: %v", err)
	}
	for _, name := range []string{"revision_lookup_source", "revision_attach", "revision_save"} {
		if !hasStage(hook.stages, name) {
			t.Errorf("expected stage %q; got %v", name, stageNames(hook.stages))
		}
	}
	created, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != FactNote {
		t.Errorf("Contest fact kind = %v, want FactNote", created.Kind)
	}
	rev, ok := domain.RevisionOf(created)
	if !ok || rev.Kind != domain.RevisionContest || rev.SourceFactID != first.FactIDs[0] {
		t.Errorf("Revision = %+v ok=%v, want contest/%s", rev, ok, first.FactIDs[0])
	}
}
