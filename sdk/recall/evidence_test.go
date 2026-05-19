package recall

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	evidencestore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// failingEvidence wraps an evidence store and lets a test force
// Append to fail so the Save rollback path can be exercised.
type failingEvidence struct {
	evidencestore.Store
	failAppend bool
	appended   int
}

func (f *failingEvidence) Append(ctx context.Context, scope model.Scope, factID string, refs []model.EvidenceRef) error {
	if f.failAppend {
		return errors.New("evidence backend down")
	}
	f.appended++
	return f.Store.Append(ctx, scope, factID, refs)
}

// ---------------------------------------------------------------
// Save mirror-write
// ---------------------------------------------------------------

func TestSave_MirrorsEvidenceWhenStoreConfigured(t *testing.T) {
	ev := evidencestore.NewMemoryStore()
	mem, err := New(withEvidenceStore(ev))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "alice met bob",
			EvidenceRefs: []EvidenceRef{
				{ID: "ev1", MessageID: "m1", Text: "alice met bob at 6pm"},
				{ID: "ev2", MessageID: "m1", Text: "at noon"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := ev.ListByFact(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "ev1" || got[1].ID != "ev2" {
		t.Errorf("evidence store missing mirrored refs: %+v", got)
	}
}

func TestSave_NoEvidenceStore_DoesNotPanic(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "alice",
			EvidenceRefs: []EvidenceRef{{Text: "raw"}},
		}},
	})
	if err != nil {
		t.Errorf("save without evidence store must not error: %v", err)
	}
}

func TestSave_EvidenceFailureRollsBackCanonicalFact(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	ev := &failingEvidence{Store: evidencestore.NewMemoryStore(), failAppend: true}
	idx := retrievalmem.New()
	mem, err := New(
		withTemporalStore(store),
		withEvidenceStore(ev),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "x",
			EvidenceRefs: []EvidenceRef{{ID: "ev1", Text: "raw"}},
		}},
	})
	if err == nil {
		t.Fatal("expected evidence-mirror failure")
	}
	// Canonical store must be empty — rollback deleted the fact.
	facts, err := store.List(context.Background(), scope, temporalstore.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Errorf("canonical store not rolled back: %+v", facts)
	}
	// Retrieval projection must also be clean.
	if _, ok, _ := idx.Get(context.Background(), retrievalproj.NamespaceFor(scope), "any"); ok {
		t.Errorf("retrieval projection should be empty after rollback")
	}
}

// ---------------------------------------------------------------
// Forget evidence sweep
// ---------------------------------------------------------------

func TestForget_SweepsEvidenceAdapter(t *testing.T) {
	ev := evidencestore.NewMemoryStore()
	mem, err := New(withEvidenceStore(ev))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "x",
			EvidenceRefs: []EvidenceRef{{ID: "ev1", Text: "raw"}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := mem.Forget(context.Background(), scope, res.FactIDs[0]); err != nil {
		t.Fatalf("forget: %v", err)
	}
	got, _ := ev.ListByFact(context.Background(), scope, res.FactIDs[0])
	if len(got) != 0 {
		t.Errorf("evidence not swept: %+v", got)
	}
}

// ---------------------------------------------------------------
// GetEvidence: store + fallback
// ---------------------------------------------------------------

func TestGetEvidence_PrefersAdapter(t *testing.T) {
	ev := evidencestore.NewMemoryStore()
	mem, err := New(withEvidenceStore(ev))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "x",
			EvidenceRefs: []EvidenceRef{{ID: "ev1", Text: "raw"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup, ok := mem.(EvidenceLookup)
	if !ok {
		t.Fatal("Memory must satisfy EvidenceLookup")
	}
	got, err := lookup.GetEvidence(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ev1" {
		t.Errorf("adapter lookup returned %+v", got)
	}
}

func TestGetEvidence_FallsBackToEmbeddedWhenNoStore(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "x",
			EvidenceRefs: []EvidenceRef{{ID: "ev1", Text: "raw"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mem.(EvidenceLookup).GetEvidence(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ev1" {
		t.Errorf("embedded fallback returned %+v", got)
	}
}

func TestGetEvidence_MissingFactReturnsNilNotError(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	got, err := mem.(EvidenceLookup).GetEvidence(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, "missing")
	if err != nil {
		t.Errorf("missing fact must not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing fact must return nil refs, got %+v", got)
	}
}

func TestGetEvidence_ValidationErrorsClassified(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	lookup := mem.(EvidenceLookup)
	if _, err := lookup.GetEvidence(context.Background(), Scope{}, "x"); !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id: %v", err)
	}
	if _, err := lookup.GetEvidence(context.Background(), Scope{RuntimeID: "rt"}, ""); !errdefs.IsValidation(err) {
		t.Errorf("missing fact id: %v", err)
	}
}

// ---------------------------------------------------------------
// Evidence rebuild — exact-replace semantics
// ---------------------------------------------------------------

func TestRebuildAll_RehydratesEvidenceAdapter(t *testing.T) {
	ev := evidencestore.NewMemoryStore()
	mem, err := New(withEvidenceStore(ev))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:         FactNote,
			Content:      "x",
			EvidenceRefs: []EvidenceRef{{ID: "ev1", Text: "raw"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.ForgetByFact(context.Background(), scope, res.FactIDs); err != nil {
		t.Fatal(err)
	}
	if err := mem.(ProjectionRebuilder).RebuildAll(context.Background(), scope); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	got, _ := ev.ListByFact(context.Background(), scope, res.FactIDs[0])
	if len(got) != 1 || got[0].ID != "ev1" {
		t.Errorf("evidence not rehydrated: %+v", got)
	}
}

func TestRebuildAll_RemovesEvidenceForDeletedFacts(t *testing.T) {
	ev := evidencestore.NewMemoryStore()
	mem, err := New(withEvidenceStore(ev))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	// Seed adapter-only evidence for a fact that is not present in
	// the canonical temporal store. A full evidence rebuild must
	// remove it because embedded evidence on TemporalFact is the
	// authoritative source.
	if err := ev.Append(context.Background(), scope, "stale-fact", []EvidenceRef{{ID: "stale-ev", Text: "orphan"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ev.ListByFact(context.Background(), scope, "stale-fact"); len(got) != 1 {
		t.Fatalf("setup failed, expected stale evidence, got %+v", got)
	}

	if err := mem.(ProjectionRebuilder).RebuildAll(context.Background(), scope); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got, _ := ev.ListByFact(context.Background(), scope, "stale-fact"); len(got) != 0 {
		t.Fatalf("RebuildAll must remove evidence for facts absent from canonical store, got %+v", got)
	}
}
