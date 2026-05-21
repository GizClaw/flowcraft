package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/diagnostics"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	retrievallens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// captureHook records OnStage diagnostics from pipeline runs.
type captureHook struct {
	telemetry.NopHook
	stages []diagnostic.StageDiagnostic
}

func (h *captureHook) OnStage(d diagnostic.StageDiagnostic) {
	h.stages = append(h.stages, d)
}

type staleCandidateSource struct {
	id string
}

func (s staleCandidateSource) Name() string { return "retrieval" }

func (s staleCandidateSource) Query(_ context.Context, plan domain.QueryPlan) domain.SourceResult {
	return domain.SourceResult{
		Source: s.Name(),
		Candidates: []domain.Candidate{{
			FactID: s.id,
			Scope:  plan.Intent.Scope,
			Source: s.Name(),
			Rank:   1,
			Score:  1,
		}},
	}
}

// ---------------------------------------------------------------
// RebuildAll
// ---------------------------------------------------------------

func TestRebuildAll_RestoresRetrievalProjectionAfterDrift(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	// Simulate drift: nuke the doc from the retrieval index but
	// leave the canonical fact intact.
	if err := idx.Delete(context.Background(), retrievallens.NamespaceFor(scope), []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); ok {
		t.Fatal("setup: drift not seeded")
	}
	rb, ok := mem.(ProjectionRebuilder)
	if !ok {
		t.Fatal("Memory must satisfy ProjectionRebuilder")
	}
	if err := rb.RebuildAll(context.Background(), scope); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); !ok {
		t.Errorf("rebuild did not restore fact %s", id)
	}
}

func TestRebuildAll_DoesNotReprojectSupersededFactsToRetrieval(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Content:   "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	oldID := first.FactIDs[0]
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Content:   "Berlin",
		}},
	}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	oldFact, err := store.Get(context.Background(), scope, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if oldFact.CorrectedBy == "" {
		t.Fatalf("setup failed: old fact should be superseded, got %+v", oldFact)
	}

	// Start from a clean retrieval projection that does not contain
	// the superseded doc. RebuildAll must not put it back.
	if err := idx.Delete(context.Background(), retrievallens.NamespaceFor(scope), []string{oldID}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), oldID); ok {
		t.Fatal("setup failed: superseded doc still present before rebuild")
	}

	if err := mem.(ProjectionRebuilder).RebuildAll(context.Background(), scope); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), oldID); ok {
		t.Fatalf("RebuildAll must not reproject superseded facts into retrieval")
	}
}

func TestRebuildAll_ValidationClassifiedOnMissingRuntimeID(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	err = mem.(ProjectionRebuilder).RebuildAll(context.Background(), Scope{})
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id: %v", err)
	}
}

// ---------------------------------------------------------------
// RebuildProjection
// ---------------------------------------------------------------

func TestRebuildProjection_TargetsSingleProjection(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	if err := idx.Delete(context.Background(), retrievallens.NamespaceFor(scope), []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := mem.(ProjectionRebuilder).RebuildProjection(context.Background(), scope, "retrieval"); err != nil {
		t.Fatalf("rebuild projection: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); !ok {
		t.Errorf("retrieval projection not rebuilt for %s", id)
	}
}

func TestRebuildProjection_UnknownNameIsNotFound(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	err = mem.(ProjectionRebuilder).RebuildProjection(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, "no-such-thing")
	if !errdefs.IsNotFound(err) {
		t.Errorf("unknown projection should be NotFound: %v", err)
	}
}

func TestRebuildProjection_ValidationClassified(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	rb := mem.(ProjectionRebuilder)
	if err := rb.RebuildProjection(context.Background(), Scope{}, "retrieval"); !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id: %v", err)
	}
	if err := rb.RebuildProjection(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, ""); !errdefs.IsValidation(err) {
		t.Errorf("missing name: %v", err)
	}
}

// ---------------------------------------------------------------
// RepairStale
// ---------------------------------------------------------------

func TestRepairStale_ForgetsProjectionWithoutTouchingStore(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	if _, err := store.Get(context.Background(), scope, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); !ok {
		t.Fatal("setup: projection empty")
	}
	if err := mem.(ProjectionRebuilder).RepairStale(context.Background(), scope, []string{id}); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); ok {
		t.Errorf("RepairStale should have evicted projection entry")
	}
	if _, err := store.Get(context.Background(), scope, id); err != nil {
		t.Errorf("RepairStale must NOT touch canonical store: %v", err)
	}
}

func TestRepairStale_EmptyIDsNoop(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	if err := mem.(ProjectionRebuilder).RepairStale(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, nil); err != nil {
		t.Errorf("empty ids should be noop: %v", err)
	}
}

// ---------------------------------------------------------------
// Drift telemetry
// ---------------------------------------------------------------

func TestRecall_EmitsDriftForStaleFact(t *testing.T) {
	hook := &captureHook{}
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithRetrievalIndex(idx),
		WithTelemetryHook(hook),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	// Drift: remove canonical fact but leave the retrieval doc.
	if err := store.Delete(context.Background(), scope, []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Recall(context.Background(), scope, Query{Text: "alpha", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	expl, ok := mem.(RecallExplainer)
	if !ok {
		t.Fatal("memory must implement RecallExplainer")
	}
	_, trace, err := expl.RecallExplain(context.Background(), scope, Query{Text: "alpha", Limit: 5})
	if err != nil {
		t.Fatalf("recall explain: %v", err)
	}
	drops := diagnostics.Drops(trace)
	if len(drops) == 0 {
		t.Fatal("expected DropStaleFact in trace stages")
	}
	if drops[0].Reason != DropStaleFact || drops[0].FactID != id {
		t.Errorf("unexpected drop: %+v", drops[0])
	}
}

func TestRecall_EmitsDriftForSupersededCandidate(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithTelemetryHook(hook),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Object:    "nyc",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mem.(*memory).sources = []port.Source{staleCandidateSource{id: first.FactIDs[0]}}
	if _, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Object:    "sf",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Faulty or third-party sources can still emit stale candidates.
	// Materialize is the defense-in-depth chokepoint that drops them
	// and emits drift telemetry.
	if _, err := mem.Recall(context.Background(), scope, Query{
		Text:  "alice city",
		Limit: 5,
	}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	expl, ok := mem.(RecallExplainer)
	if !ok {
		t.Fatal("memory must implement RecallExplainer")
	}
	_, trace, err := expl.RecallExplain(context.Background(), scope, Query{Text: "alice city", Limit: 5})
	if err != nil {
		t.Fatalf("recall explain: %v", err)
	}
	found := false
	for _, d := range diagnostics.Drops(trace) {
		if d.Reason == DropSuperseded {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DropSuperseded in trace, got %+v", diagnostics.Drops(trace))
	}
}

func TestSaveRecall_EmitsPipelineTelemetry(t *testing.T) {
	hook := &captureHook{}
	mem, err := New(WithTelemetryHook(hook))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alice likes tea", Entities: []string{"alice"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Recall(context.Background(), scope, Query{Text: "Alice tea", Limit: 5}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"validate", "ingest", "resolve", "append", "validity_close",
		"project_required", "project_optional",
		"intent", "plan", "federation_fanout", "trust_filter", "rank", "build_hits",
	} {
		if !hasStage(hook.stages, want) {
			t.Fatalf("missing stage %q in %+v", want, stageNames(hook.stages))
		}
	}
}

func hasStage(stages []diagnostic.StageDiagnostic, name string) bool {
	for _, s := range stages {
		if s.Stage == name {
			return true
		}
	}
	return false
}

func stageNames(stages []diagnostic.StageDiagnostic) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Stage)
	}
	return out
}

// ---------------------------------------------------------------
// Public opt-in interface assertions
// ---------------------------------------------------------------

func TestMemory_ImplementsOptInInterfaces(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if _, ok := mem.(ProjectionRebuilder); !ok {
		t.Errorf("Memory must satisfy ProjectionRebuilder")
	}
	if _, ok := mem.(EvidenceLookup); !ok {
		t.Errorf("Memory must satisfy EvidenceLookup")
	}
	if _, ok := mem.(RecallExplainer); !ok {
		t.Errorf("Memory must satisfy RecallExplainer")
	}
}
