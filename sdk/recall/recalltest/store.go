// Package recalltest contains reusable conformance suites for sdk/recall
// adapter implementations.
package recalltest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

// TemporalStoreFactory returns a fresh, empty TemporalStore for one subtest.
type TemporalStoreFactory func(t testing.TB) recall.TemporalStore

// RunTemporalStoreSuite verifies the public TemporalStore adapter contract.
func RunTemporalStoreSuite(t *testing.T, newStore TemporalStoreFactory) {
	t.Helper()

	t.Run("append rejects duplicate id", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		f := temporalFact("a", "k", recall.FactNote, time.Unix(1, 0))
		if err := store.Append(ctx, []recall.TemporalFact{f}); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if err := store.Append(ctx, []recall.TemporalFact{f}); err == nil {
			t.Fatal("want duplicate id error")
		}
	})

	t.Run("append batch is atomic on duplicate ids", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		a := temporalFact("dup", "k1", recall.FactNote, time.Unix(1, 0))
		b := temporalFact("dup", "k2", recall.FactNote, time.Unix(2, 0))
		if err := store.Append(ctx, []recall.TemporalFact{a, b}); err == nil {
			t.Fatal("want duplicate id error")
		}
		got, err := store.List(ctx, conformanceScope(), recall.ListQuery{IncludeSuperseded: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("failed append batch must not partially commit facts: %+v", got)
		}
	})

	t.Run("append validates kind and runtime scope", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, []recall.TemporalFact{{ID: "x", Scope: conformanceScope(), Kind: "bogus"}}); err == nil {
			t.Fatal("want invalid kind error")
		}
		if err := store.Append(ctx, []recall.TemporalFact{{ID: "x", Kind: recall.FactNote}}); err == nil {
			t.Fatal("want missing runtime_id error")
		}
	})

	t.Run("list filters and orders", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		facts := []recall.TemporalFact{
			temporalFact("b", "kb", recall.FactNote, time.Unix(20, 0), "alice", "bob"),
			temporalFact("a", "ka", recall.FactState, time.Unix(10, 0), "alice"),
			temporalFact("c", "kc", recall.FactState, time.Unix(30, 0), "carol"),
		}
		if err := store.Append(ctx, facts); err != nil {
			t.Fatal(err)
		}

		states, err := store.List(ctx, conformanceScope(), recall.ListQuery{Kinds: []recall.FactKind{recall.FactState}})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(states) != "a,c" {
			t.Fatalf("state list ids = %s, want a,c", gotIDs(states))
		}

		aliceAndBob, err := store.List(ctx, conformanceScope(), recall.ListQuery{Entities: []string{"alice", "bob"}})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(aliceAndBob) != "b" {
			t.Fatalf("entity-filtered ids = %s, want b", gotIDs(aliceAndBob))
		}

		limited, err := store.List(ctx, conformanceScope(), recall.ListQuery{IncludeSuperseded: true, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(limited) != "a,b" {
			t.Fatalf("limited ordered ids = %s, want a,b", gotIDs(limited))
		}
	})

	t.Run("validity close hide and reopen", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		a := temporalFact("a", "k", recall.FactState, time.Unix(1, 0))
		b := temporalFact("b", "k", recall.FactState, time.Unix(2, 0))
		b.Supersedes = []string{"a"}
		if err := store.Append(ctx, []recall.TemporalFact{a, b}); err != nil {
			t.Fatal(err)
		}

		validTo := time.Unix(2, 0)
		if err := store.UpdateValidity(ctx, conformanceScope(), "a", validTo, "b"); err != nil {
			t.Fatalf("UpdateValidity: %v", err)
		}
		if err := store.UpdateValidity(ctx, conformanceScope(), "a", validTo, "b"); err != nil {
			t.Fatalf("idempotent UpdateValidity: %v", err)
		}
		if err := store.UpdateValidity(ctx, conformanceScope(), "a", time.Unix(3, 0), "c"); !errors.Is(err, recall.ErrTemporalValidityAlreadyClosed) {
			t.Fatalf("re-close error = %v, want ErrTemporalValidityAlreadyClosed", err)
		}

		active, err := store.List(ctx, conformanceScope(), recall.ListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(active) != "b" {
			t.Fatalf("active ids = %s, want b", gotIDs(active))
		}
		supersededBy, err := store.FindSupersededBy(ctx, conformanceScope(), "b")
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(supersededBy) != "a" {
			t.Fatalf("FindSupersededBy = %s, want a", gotIDs(supersededBy))
		}

		if err := store.ReopenValidity(ctx, conformanceScope(), "a", "wrong"); !errors.Is(err, recall.ErrTemporalReopenConflict) {
			t.Fatalf("wrong reopen error = %v, want ErrTemporalReopenConflict", err)
		}
		if err := store.ReopenValidity(ctx, conformanceScope(), "a", "b"); err != nil {
			t.Fatalf("ReopenValidity: %v", err)
		}
		reopened, err := store.Get(ctx, conformanceScope(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if reopened.ValidTo != nil || reopened.CorrectedBy != "" {
			t.Fatalf("reopened fact still closed: %+v", reopened)
		}
	})

	t.Run("finds by merge key and origin request", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		a := temporalFact("a", "k", recall.FactEpisode, time.Unix(1, 0))
		a.Origin = recall.FactOrigin{RequestID: "req-1", Kind: recall.OriginKindEpisode}
		b := temporalFact("b", "k", recall.FactEpisode, time.Unix(2, 0))
		b.Origin = recall.FactOrigin{RequestID: "req-1", Kind: recall.OriginKindEpisode}
		c := temporalFact("c", "other", recall.FactNote, time.Unix(3, 0))
		if err := store.Append(ctx, []recall.TemporalFact{a, b, c}); err != nil {
			t.Fatal(err)
		}
		byMerge, err := store.FindByMergeKey(ctx, conformanceScope(), "k")
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(byMerge) != "a,b" {
			t.Fatalf("FindByMergeKey = %s, want a,b", gotIDs(byMerge))
		}
		if empty, err := store.FindByMergeKey(ctx, conformanceScope(), ""); err != nil || len(empty) != 0 {
			t.Fatalf("empty merge key = %+v err=%v, want empty nil-error", empty, err)
		}

		byOrigin, err := store.FindByOriginRequestID(ctx, conformanceScope(), "req-1")
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(byOrigin) != "a,b" {
			t.Fatalf("FindByOriginRequestID = %s, want a,b", gotIDs(byOrigin))
		}
		if empty, err := store.FindByOriginRequestID(ctx, conformanceScope(), ""); err != nil || len(empty) != 0 {
			t.Fatalf("empty origin request = %+v err=%v, want empty nil-error", empty, err)
		}
	})

	t.Run("delete prunes lookups and delete by scope isolates partition", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		other := recall.Scope{RuntimeID: "rt", UserID: "u2"}
		a := temporalFact("a", "k", recall.FactNote, time.Unix(1, 0))
		b := temporalFact("b", "k", recall.FactNote, time.Unix(2, 0))
		c := temporalFact("c", "k", recall.FactNote, time.Unix(3, 0))
		c.Scope = other
		if err := store.Append(ctx, []recall.TemporalFact{a, b, c}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, conformanceScope(), []string{"a", "missing"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.Get(ctx, conformanceScope(), "a"); !errors.Is(err, recall.ErrStoreNotFound) {
			t.Fatalf("Get deleted = %v, want ErrStoreNotFound", err)
		}
		byMerge, err := store.FindByMergeKey(ctx, conformanceScope(), "k")
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(byMerge) != "b" {
			t.Fatalf("merge key after delete = %s, want b", gotIDs(byMerge))
		}
		n, err := store.DeleteByScope(ctx, conformanceScope())
		if err != nil {
			t.Fatalf("DeleteByScope: %v", err)
		}
		if n != 1 {
			t.Fatalf("DeleteByScope removed %d, want 1", n)
		}
		remaining, err := store.List(ctx, other, recall.ListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(remaining) != "c" {
			t.Fatalf("other partition after DeleteByScope = %s, want c", gotIDs(remaining))
		}
	})

	t.Run("feedback closed and history chain", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		a := temporalFact("a", "k", recall.FactState, time.Unix(1, 0))
		b := temporalFact("b", "k", recall.FactState, time.Unix(2, 0))
		b.Supersedes = []string{"a"}
		if err := store.Append(ctx, []recall.TemporalFact{a, b}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateValidity(ctx, conformanceScope(), "a", time.Unix(2, 0), "b"); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateFeedback(ctx, conformanceScope(), "b", 2, 0.5); err != nil {
			t.Fatalf("UpdateFeedback: %v", err)
		}
		if err := store.UpdateFeedback(ctx, conformanceScope(), "b", -5, -5); err != nil {
			t.Fatalf("UpdateFeedback clamp: %v", err)
		}
		got, err := store.Get(ctx, conformanceScope(), "b")
		if err != nil {
			t.Fatal(err)
		}
		if got.Reinforcement != 0 || got.Penalty != 0 {
			t.Fatalf("feedback should clamp to zero, got reinforcement=%v penalty=%v", got.Reinforcement, got.Penalty)
		}
		if err := store.MarkClosed(ctx, conformanceScope(), "b", true); err != nil {
			t.Fatalf("MarkClosed: %v", err)
		}
		got, _ = store.Get(ctx, conformanceScope(), "b")
		if !got.Closed {
			t.Fatal("MarkClosed(true) did not persist")
		}
		history, err := store.ListByID(ctx, conformanceScope(), "b")
		if err != nil {
			t.Fatalf("ListByID: %v", err)
		}
		if gotIDs(history) != "a,b" {
			t.Fatalf("ListByID ids = %s, want a,b", gotIDs(history))
		}
	})

	t.Run("scope partition excludes agent id", func(t *testing.T) {
		store := temporalStoreForTest(t, newStore)
		ctx := context.Background()
		a := temporalFact("a", "ka", recall.FactNote, time.Unix(1, 0))
		a.Scope.AgentID = "agent-a"
		b := temporalFact("b", "kb", recall.FactNote, time.Unix(2, 0))
		b.Scope.AgentID = "agent-b"
		if err := store.Append(ctx, []recall.TemporalFact{a, b}); err != nil {
			t.Fatal(err)
		}
		got, err := store.List(ctx, recall.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}, recall.ListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if gotIDs(got) != "a,b" {
			t.Fatalf("agent id must not hard-partition store, got %s", gotIDs(got))
		}
	})
}

func temporalStoreForTest(t testing.TB, newStore TemporalStoreFactory) recall.TemporalStore {
	t.Helper()
	store := newStore(t)
	if store == nil {
		t.Fatal("TemporalStoreFactory returned nil")
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("TemporalStore.Close: %v", err)
		}
	})
	return store
}

func temporalFact(id, mergeKey string, kind recall.FactKind, observedAt time.Time, entities ...string) recall.TemporalFact {
	return recall.TemporalFact{
		ID:         id,
		Scope:      conformanceScope(),
		Kind:       kind,
		Content:    "content-" + id,
		MergeKey:   mergeKey,
		Entities:   entities,
		ObservedAt: observedAt,
	}
}
