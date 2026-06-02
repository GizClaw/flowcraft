package recalltest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// EvidenceStoreFactory returns a fresh, empty EvidenceStore for one subtest.
type EvidenceStoreFactory func(t testing.TB) recall.EvidenceStore

// RunEvidenceStoreSuite verifies the public EvidenceStore adapter contract.
func RunEvidenceStoreSuite(t *testing.T, newStore EvidenceStoreFactory) {
	t.Helper()

	t.Run("append persists and returns refs in order", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{
			evidenceRef("e1", "m1", "hello", 10),
			evidenceRef("e2", "m1", "world", 11),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := store.ListByFact(ctx, conformanceScope(), "f1")
		if err != nil {
			t.Fatalf("ListByFact: %v", err)
		}
		if evidenceIDs(got) != "e1,e2" {
			t.Fatalf("ListByFact ids = %s, want e1,e2", evidenceIDs(got))
		}
	})

	t.Run("append assigns stable ids for empty refs", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		refs := []recall.EvidenceRef{{Text: "first"}, {Text: "second"}}
		if err := store.Append(ctx, conformanceScope(), "f1", refs); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := store.ListByFact(ctx, conformanceScope(), "f1")
		if err != nil {
			t.Fatalf("ListByFact: %v", err)
		}
		if evidenceIDs(got) != "f1#0,f1#1" {
			t.Fatalf("stable auto ids = %s, want f1#0,f1#1", evidenceIDs(got))
		}
		if err := store.Append(ctx, conformanceScope(), "f1", refs); err != nil {
			t.Fatalf("replay Append: %v", err)
		}
		again, err := store.ListByFact(ctx, conformanceScope(), "f1")
		if err != nil {
			t.Fatalf("ListByFact replay: %v", err)
		}
		if evidenceIDs(again) != "f1#0,f1#1" {
			t.Fatalf("replay duplicated refs: %+v", again)
		}
	})

	t.Run("append replay updates payload without duplicating index", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{
			evidenceRef("e1", "m1", "v1", 1),
		}); err != nil {
			t.Fatalf("Append 1: %v", err)
		}
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{
			evidenceRef("e1", "m1", "v2", 2),
		}); err != nil {
			t.Fatalf("Append 2: %v", err)
		}
		got, err := store.ListByFact(ctx, conformanceScope(), "f1")
		if err != nil {
			t.Fatalf("ListByFact: %v", err)
		}
		if len(got) != 1 || got[0].Text != "v2" {
			t.Fatalf("replay should update one ref payload, got %+v", got)
		}
	})

	t.Run("shared evidence id is isolated by fact", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{
			evidenceRef("turn-1", "m1", "fact one quote", 1),
		}); err != nil {
			t.Fatalf("Append f1: %v", err)
		}
		if err := store.Append(ctx, conformanceScope(), "f2", []recall.EvidenceRef{
			evidenceRef("turn-1", "m1", "fact two quote", 2),
		}); err != nil {
			t.Fatalf("Append f2: %v", err)
		}
		f1, err := store.ListByFact(ctx, conformanceScope(), "f1")
		if err != nil {
			t.Fatalf("ListByFact f1: %v", err)
		}
		f2, err := store.ListByFact(ctx, conformanceScope(), "f2")
		if err != nil {
			t.Fatalf("ListByFact f2: %v", err)
		}
		if len(f1) != 1 || f1[0].Text != "fact one quote" {
			t.Fatalf("f1 refs = %+v", f1)
		}
		if len(f2) != 1 || f2[0].Text != "fact two quote" {
			t.Fatalf("f2 refs = %+v", f2)
		}
	})

	t.Run("validation and not found errors are classified", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, recall.Scope{}, "f1", []recall.EvidenceRef{evidenceRef("e1", "", "", 0)}); !errdefs.IsValidation(err) {
			t.Fatalf("missing runtime_id error = %v, want validation", err)
		}
		if err := store.Append(ctx, conformanceScope(), "", []recall.EvidenceRef{evidenceRef("e1", "", "", 0)}); !errdefs.IsValidation(err) {
			t.Fatalf("missing fact id error = %v, want validation", err)
		}
		if _, err := store.Get(ctx, conformanceScope(), "missing"); !errdefs.IsNotFound(err) {
			t.Fatalf("missing evidence error = %v, want not found", err)
		}
	})

	t.Run("empty fact id does not enumerate scope", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{evidenceRef("e1", "", "", 0)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := store.ListByFact(ctx, conformanceScope(), "")
		if err != nil {
			t.Fatalf("ListByFact empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty fact id must not enumerate scope, got %+v", got)
		}
	})

	t.Run("list fact ids and forget are scoped", func(t *testing.T) {
		store := evidenceStoreForTest(t, newStore)
		ctx := context.Background()
		other := recall.Scope{RuntimeID: "rt", UserID: "u2"}
		if err := store.Append(ctx, conformanceScope(), "f1", []recall.EvidenceRef{
			evidenceRef("e1", "", "", 0),
			evidenceRef("e2", "", "", 1),
		}); err != nil {
			t.Fatalf("Append f1: %v", err)
		}
		if err := store.Append(ctx, conformanceScope(), "f2", []recall.EvidenceRef{evidenceRef("e3", "", "", 2)}); err != nil {
			t.Fatalf("Append f2: %v", err)
		}
		if err := store.Append(ctx, other, "f1", []recall.EvidenceRef{evidenceRef("e1", "", "other", 3)}); err != nil {
			t.Fatalf("Append other: %v", err)
		}
		factIDs, err := store.ListFactIDs(ctx, conformanceScope())
		if err != nil {
			t.Fatalf("ListFactIDs: %v", err)
		}
		if !sameStringSet(factIDs, []string{"f1", "f2"}) {
			t.Fatalf("ListFactIDs = %+v, want f1/f2", factIDs)
		}

		if err := store.ForgetByFact(ctx, conformanceScope(), []string{"f1", "missing"}); err != nil {
			t.Fatalf("ForgetByFact: %v", err)
		}
		if got, err := store.ListByFact(ctx, conformanceScope(), "f1"); err != nil || len(got) != 0 {
			t.Fatalf("f1 after forget = %+v err=%v, want empty", got, err)
		}
		if _, err := store.Get(ctx, conformanceScope(), "e1"); !errdefs.IsNotFound(err) {
			t.Fatalf("forgotten evidence error = %v, want not found", err)
		}
		if got, err := store.ListByFact(ctx, conformanceScope(), "f2"); err != nil || evidenceIDs(got) != "e3" {
			t.Fatalf("f2 after f1 forget = %+v err=%v, want e3", got, err)
		}
		if got, err := store.ListByFact(ctx, other, "f1"); err != nil || len(got) != 1 || got[0].Text != "other" {
			t.Fatalf("other scope f1 = %+v err=%v, want untouched other ref", got, err)
		}
	})
}

func evidenceStoreForTest(t testing.TB, newStore EvidenceStoreFactory) recall.EvidenceStore {
	t.Helper()
	store := newStore(t)
	if store == nil {
		t.Fatal("EvidenceStoreFactory returned nil")
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("EvidenceStore.Close: %v", err)
		}
	})
	return store
}

func evidenceRef(id, messageID, text string, ts int64) recall.EvidenceRef {
	return recall.EvidenceRef{
		ID:        id,
		MessageID: messageID,
		Role:      "user",
		Text:      text,
		Timestamp: time.Unix(ts, 0),
	}
}

func evidenceIDs(refs []recall.EvidenceRef) string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return stringsJoin(ids)
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, value := range got {
		seen[value]++
	}
	for _, value := range want {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}
