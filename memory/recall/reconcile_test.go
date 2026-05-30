package recall

import (
	"context"
	"errors"
	"testing"
	"time"

	retrievallens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestReconcileRuntime_RebuildsEnumeratedScopes(t *testing.T) {
	ctx := context.Background()
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithRetrievalIndex(idx))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	u1 := Scope{RuntimeID: "rt", UserID: "u1"}
	u2 := Scope{RuntimeID: "rt", UserID: "u2"}
	r1, err := mem.Save(ctx, u1, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mem.Save(ctx, u2, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "beta"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, retrievallens.NamespaceFor(u1), r1.FactIDs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, retrievallens.NamespaceFor(u2), r2.FactIDs); err != nil {
		t.Fatal(err)
	}

	rec, ok := NewReconciler(mem)
	if !ok {
		t.Fatal("Memory must expose Reconciler")
	}
	got, err := rec.ReconcileRuntime(ctx, "rt", ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Scopes != 2 || got.Rebuilt != 2 || got.Failed != 0 {
		t.Fatalf("result = %+v, want two rebuilt scopes", got)
	}
	for _, row := range got.Results {
		if row.FactsScanned != 1 {
			t.Fatalf("row FactsScanned = %d, want 1: %+v", row.FactsScanned, row)
		}
	}
	if _, ok, _ := idx.Get(ctx, retrievallens.NamespaceFor(u1), r1.FactIDs[0]); !ok {
		t.Fatal("u1 projection was not rebuilt")
	}
	if _, ok, _ := idx.Get(ctx, retrievallens.NamespaceFor(u2), r2.FactIDs[0]); !ok {
		t.Fatal("u2 projection was not rebuilt")
	}
}

func TestReconcileSideEffects_RestoresProjectionFromCanonicalFacts(t *testing.T) {
	ctx := context.Background()
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithRetrievalIndex(idx))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	res, err := mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, retrievallens.NamespaceFor(scope), res.FactIDs); err != nil {
		t.Fatal(err)
	}

	rec, _ := NewReconciler(mem)
	got, err := rec.ReconcileSideEffects(ctx, scope, SideEffectReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile side effects: %v", err)
	}
	if !got.Rebuilt || got.FactsScanned != 1 || got.Scope.AgentID != "" {
		t.Fatalf("result = %+v, want rebuilt hard partition with one fact", got)
	}
	if _, ok, _ := idx.Get(ctx, retrievallens.NamespaceFor(scope), res.FactIDs[0]); !ok {
		t.Fatal("projection was not restored")
	}
}

func TestReconcileSideEffects_TargetsProjection(t *testing.T) {
	ctx := context.Background()
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	rec, _ := NewReconciler(mem)

	got, err := rec.ReconcileSideEffects(ctx, Scope{RuntimeID: "rt", UserID: "u1"}, SideEffectReconcileOptions{
		ProjectionName: "retrieval",
	})
	if err != nil {
		t.Fatalf("targeted reconcile: %v", err)
	}
	if !got.Rebuilt || got.ProjectionName != "retrieval" {
		t.Fatalf("result = %+v, want targeted rebuild", got)
	}
	if _, err := rec.ReconcileSideEffects(ctx, Scope{RuntimeID: "rt", UserID: "u1"}, SideEffectReconcileOptions{
		ProjectionName: "missing",
	}); !errdefs.IsNotFound(err) {
		t.Fatalf("missing projection error = %v, want not found", err)
	}
}

func TestReconcileAsyncSemantic_ReportsPendingAndCompleted(t *testing.T) {
	ctx := context.Background()
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(WithTemporalStore(store), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	save, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "I like tea"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := NewReconciler(mem)
	pending, err := rec.ReconcileAsyncSemantic(ctx, scope, AsyncSemanticReconcileOptions{})
	if err != nil {
		t.Fatalf("pending reconcile: %v", err)
	}
	if pending.Episodes != 1 || pending.Pending != 1 || pending.Completed != 0 {
		t.Fatalf("pending result = %+v, want one pending request", pending)
	}

	semantic := TemporalFact{
		ID:         "sem-1",
		Scope:      scope,
		Kind:       FactNote,
		Content:    "Alice likes tea.",
		ObservedAt: time.Unix(10, 0),
		Origin: FactOrigin{
			RequestID:      save.AsyncRequestID,
			Kind:           OriginKindSemanticDerivation,
			EpisodeFactIDs: append([]string(nil), save.EpisodeFactIDs...),
		},
	}
	if err := store.Append(ctx, []TemporalFact{semantic}); err != nil {
		t.Fatal(err)
	}
	completed, err := rec.ReconcileAsyncSemantic(ctx, scope, AsyncSemanticReconcileOptions{})
	if err != nil {
		t.Fatalf("completed reconcile: %v", err)
	}
	if completed.Completed != 1 || completed.Pending != 0 {
		t.Fatalf("completed result = %+v, want one completed request", completed)
	}
	if got := completed.Results[0].SemanticFactIDs; len(got) != 1 || got[0] != "sem-1" {
		t.Fatalf("semantic ids = %v, want [sem-1]", got)
	}
}

func TestReconcileAsyncSemantic_ClassifiesSkippedAndUnrecoverable(t *testing.T) {
	ctx := context.Background()
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	expiredAt := time.Unix(10, 0)
	facts := []TemporalFact{
		{
			ID:         "ep-missing-origin",
			Scope:      scope,
			Kind:       FactEpisode,
			Content:    "Alice: hi",
			ObservedAt: time.Unix(1, 0),
		},
		{
			ID:         "ep-expired",
			Scope:      scope,
			Kind:       FactEpisode,
			Content:    "Bob: bye",
			ObservedAt: time.Unix(2, 0),
			ExpiresAt:  &expiredAt,
			Origin: FactOrigin{
				RequestID: "areq-expired",
				Kind:      OriginKindEpisode,
			},
		},
	}
	if err := store.Append(ctx, facts); err != nil {
		t.Fatal(err)
	}
	rec, _ := NewReconciler(mem)
	got, err := rec.ReconcileAsyncSemantic(ctx, scope, AsyncSemanticReconcileOptions{Now: time.Unix(11, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != 1 || got.Unrecoverable != 1 || got.Pending != 0 || got.Completed != 0 {
		t.Fatalf("result = %+v, want skipped=1 unrecoverable=1", got)
	}
}

func TestReconcileScopes_DoesNotExpandFederation(t *testing.T) {
	ctx := context.Background()
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithRetrievalIndex(idx))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	user := Scope{RuntimeID: "rt", UserID: "u1"}
	global := Scope{RuntimeID: "rt"}
	userRes, err := mem.Save(ctx, user, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "user fact"}}})
	if err != nil {
		t.Fatal(err)
	}
	globalRes, err := mem.Save(ctx, global, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "global fact"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, retrievallens.NamespaceFor(user), userRes.FactIDs); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(ctx, retrievallens.NamespaceFor(global), globalRes.FactIDs); err != nil {
		t.Fatal(err)
	}

	rec, _ := NewReconciler(mem)
	got, err := rec.ReconcileScopes(ctx, []Scope{
		{RuntimeID: "rt", UserID: "u1", Federation: []Scope{{RuntimeID: "rt"}}},
		{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"},
	}, ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Scopes != 1 || got.Rebuilt != 1 {
		t.Fatalf("result = %+v, want exactly one primary partition", got)
	}
	if _, ok, _ := idx.Get(ctx, retrievallens.NamespaceFor(user), userRes.FactIDs[0]); !ok {
		t.Fatal("primary user projection was not rebuilt")
	}
	if _, ok, _ := idx.Get(ctx, retrievallens.NamespaceFor(global), globalRes.FactIDs[0]); ok {
		t.Fatal("federated global scope must not be rebuilt implicitly")
	}
}

func TestReconcileScopes_ExpireRetiredBeforeRebuild(t *testing.T) {
	ctx := context.Background()
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	expiresAt := time.Unix(10, 0)
	res, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "old", ExpiresAt: &expiresAt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := NewReconciler(mem)
	got, err := rec.ReconcileScopes(ctx, []Scope{scope}, ReconcileOptions{
		ExpireRetired: true,
		Now:           time.Unix(11, 0),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Expired != 1 || got.Rebuilt != 1 {
		t.Fatalf("result = %+v, want expired=1 rebuilt=1", got)
	}
	if _, err := store.Get(ctx, scope, res.FactIDs[0]); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("expired fact err = %v, want ErrStoreNotFound", err)
	}
}

func TestReconcile_Validation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	rec, _ := NewReconciler(mem)
	if _, err := rec.ReconcileRuntime(context.Background(), "", ReconcileOptions{}); !errdefs.IsValidation(err) {
		t.Fatalf("empty runtime error = %v, want validation", err)
	}
	if _, err := rec.ReconcileScopes(context.Background(), []Scope{{}}, ReconcileOptions{}); !errdefs.IsValidation(err) {
		t.Fatalf("empty scope error = %v, want validation", err)
	}
}

func TestReconcileError_UnwrapsScopeFailures(t *testing.T) {
	sentinel := errors.New("boom")
	err := ReconcileError{Failures: []ScopeReconcileFailure{{
		Scope: Scope{RuntimeID: "rt", UserID: "u1"},
		Err:   sentinel,
	}}}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is did not see per-scope failure: %v", err)
	}
}
