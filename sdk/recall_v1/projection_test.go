package recall_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall_v1"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// TestReconciler_AddEagerlyLinksEntityStore pins the post-#179.1
// contract: both Memory.Save and Memory.Add fan the just-written
// entries out through [recall.Projection.Project] inline, so the
// entity-link inverted index is populated with 0 lag — no Reconciler
// tick required for caller-visible recall on own writes. A
// subsequent SyncSideStores tick MUST be a no-op (idempotent
// additive Project semantics), which doubles as the regression
// guard against re-introducing the pre-#171 Add↔Save asymmetry
// where Add quietly skipped the projection.
func TestReconciler_AddEagerlyLinksEntityStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithEntityStore(0),
		// Disable the background loop so the test deterministically
		// owns when (if at all) reconciliation runs.
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	id, err := m.Add(ctx, scope, recall.Entry{
		Content:  "Alice met Bob at the library",
		Entities: []string{"alice", "bob"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Eager Project: store sees the link the moment Add returns.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if len(got) != 1 || got[0] != id {
		t.Fatalf("alice link missing after Add (eager projection); got %v want [%q]", got, id)
	}
	gotBob, _ := store.Lookup(ctx, scope, []string{"bob"}, 0)
	if len(gotBob) != 1 || gotBob[0] != id {
		t.Fatalf("bob link missing after Add (eager projection); got %v want [%q]", gotBob, id)
	}

	// And SyncSideStores is idempotent against the eager write:
	// replaying Project for the same entry must not duplicate the
	// edge nor change Lookup output.
	syncer, ok := m.(recall.SideStoreSyncer)
	if !ok {
		t.Fatalf("Memory does not implement SideStoreSyncer")
	}
	if err := syncer.SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores: %v", err)
	}
	got, _ = store.Lookup(ctx, scope, []string{"alice"}, 0)
	if len(got) != 1 || got[0] != id {
		t.Fatalf("alice link diverged after idempotent reconcile; got %v want [%q]", got, id)
	}
	gotBob, _ = store.Lookup(ctx, scope, []string{"bob"}, 0)
	if len(gotBob) != 1 || gotBob[0] != id {
		t.Fatalf("bob link diverged after idempotent reconcile; got %v want [%q]", gotBob, id)
	}
}

// TestReconciler_RollbackPrunesEntityStore pins issue #164: an
// Auditable.Rollback that removes an entry from the primary index
// leaves the entity-link inverted index pointing at the now-dead
// id. The Reconciler must drop those references during a sync.
func TestReconciler_RollbackPrunesEntityStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"Alice loves jazz","entities":["Alice","jazz"]}]`
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEntityStore(0),
		recall.WithJournal(journal.NewMemoryJournal()),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	res, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Alice loves jazz"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.EntryIDs) == 0 {
		t.Fatalf("Save returned no entries")
	}
	id := res.EntryIDs[0]

	// Pre-rollback: store has the link.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if len(got) != 1 {
		t.Fatalf("pre-rollback alice link missing; got %v", got)
	}

	// Roll back to a time before the entry existed — Auditable
	// removes it from primary but does NOT touch the entity store
	// today (issue #164).
	aud := m.(recall.Auditable)
	if err := aud.Rollback(ctx, scope, id, time.Time{}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Direct probe: pre-fix #164 leaves the link pointing at the
	// dead id. We don't assert on that state — only that the
	// post-reconcile state is clean.

	if err := m.(recall.SideStoreSyncer).SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores: %v", err)
	}
	got, _ = store.Lookup(ctx, scope, []string{"alice"}, 0)
	for _, gid := range got {
		if gid == id {
			t.Fatalf("rolled-back id %q still referenced by entity store", id)
		}
	}
}

// TestReconciler_TombstonedEntryPrunedFromEntityStore pins issue
// #169: the resolver OpDelete branch writes MetaTombstone to the
// old entry but does not call EntityStore.Forget. After one
// reconcile pass the projection must drop the tombstoned id.
func TestReconciler_TombstonedEntryPrunedFromEntityStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user has a cat named Mittens", Entities: []string{"mittens", "cat"}}},
			{{Content: "user does NOT have a cat", Entities: []string{"cat"}}},
		},
	}
	var firstID string
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := newScope()
	firstIDs := saveText(t, m, scope, "I have a cat named Mittens")
	firstID = firstIDs[0]
	// Wire the second Save to fire OpDelete against firstID.
	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		return []recall.ResolveAction{{
			Op:       recall.OpDelete,
			SourceID: firstSourceID(t, batch),
			TargetID: firstID,
		}}, nil
	}
	saveText(t, m, scope, "I do not have a cat anymore")

	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	if err := m.(recall.SideStoreSyncer).SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores: %v", err)
	}
	got, _ := store.Lookup(ctx, scope, []string{"mittens"}, 0)
	for _, gid := range got {
		if gid == firstID {
			t.Fatalf("tombstoned id %q still referenced by entity store", firstID)
		}
	}
}

// TestDedupHashes_IgnoresTombstoned pins issue #158: a content
// hash that exists ONLY on tombstoned (logically deleted) docs
// must not block re-Save of the same content. The fix composes
// TombstoneFilter into dedupHashes' List filter.
func TestDedupHashes_IgnoresTombstoned(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"user owns a labrador named Lucky","entities":["lucky"]}]`
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I own a labrador named Lucky"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(first.EntryIDs) == 0 {
		t.Fatalf("Save returned no entries")
	}
	firstID := first.EntryIDs[0]
	// Tombstone the entry directly (the OpDelete resolver path
	// achieves the same metadata state).
	ns := recall.NamespaceFor(scope)
	doc, ok, err := idx.Get(ctx, ns, firstID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if doc.Metadata == nil {
		doc.Metadata = map[string]any{}
	}
	doc.Metadata[recall.MetaTombstone] = true
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{doc}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Save the same fact again — pre-fix, dedupHashes sees the
	// tombstoned hash and returns its ID, so the second Save
	// short-circuits and writes nothing. The row stays tombstoned
	// (invisible to Recall) even though the user just asked to
	// save it.
	//
	// With #158's fix, dedupHashes ignores tombstoned rows, the
	// upsertFacts path proceeds, and the deterministic entry ID
	// (which is content-derived) collides on the same row — the
	// Upsert REPLACES the tombstoned doc with a fresh one whose
	// metadata no longer carries MetaTombstone. Recall sees the
	// fact again.
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I own a labrador named Lucky"}}},
	})
	if err != nil {
		t.Fatalf("Save #2: %v", err)
	}
	if len(second.EntryIDs) == 0 {
		t.Fatalf("second Save returned no IDs; dedup may still be tombstone-blind")
	}
	revived, ok, err := idx.Get(ctx, ns, firstID)
	if err != nil || !ok {
		t.Fatalf("post-Save row missing: ok=%v err=%v", ok, err)
	}
	if v, _ := revived.Metadata[recall.MetaTombstone].(bool); v {
		t.Fatalf("post-Save row is still tombstoned; #158 fix is not effective. metadata=%v", revived.Metadata)
	}
}

// TestDedupHashes_IgnoresExpired pins issue #179.3: a content hash
// that exists ONLY on an expired (TTL'd) doc must not block re-Save
// of the same content. Pre-fix dedupHashes filtered tombstoned rows
// (#158) but not expired rows, so:
//
//   - Save returned success on the dedup_hit branch with the
//     expired row's ID.
//   - Recall composes ExpireFilter by default and silently hid the
//     row.
//   - Net effect: Save succeeded but the fact was unreachable
//     until the TTL sweeper hard-deleted the row on its next pass.
//
// The fix mirrors dedup's "alive" cutoff to Recall's reachability
// gate by composing ExpireFilter(m.cfg.now()) into the dedup query.
// We assert the post-fix invariant: re-Save of the same content
// after the previous entry expires produces a NEW, live row
// whose metadata no longer carries an in-the-past expires_at.
func TestDedupHashes_IgnoresExpired(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"user owns a labrador named Lucky","entities":["lucky"]}]`
	// Pin the clock so we can place expires_at strictly in the past
	// regardless of how slow CI machines walk wall time.
	frozen := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithClock(func() time.Time { return frozen }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I own a labrador named Lucky"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(first.EntryIDs) == 0 {
		t.Fatalf("Save returned no entries")
	}
	firstID := first.EntryIDs[0]

	// Force the freshly-written row to be already expired by
	// patching expires_at to 1ms before the (frozen) clock. This
	// reproduces the "TTL elapsed before a re-Save attempt"
	// scenario without touching the sweeper.
	ns := recall.NamespaceFor(scope)
	doc, ok, err := idx.Get(ctx, ns, firstID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if doc.Metadata == nil {
		doc.Metadata = map[string]any{}
	}
	doc.Metadata["expires_at"] = frozen.Add(-time.Millisecond).UnixMilli()
	if err := idx.Upsert(ctx, ns, []retrieval.Doc{doc}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Re-Save the same fact. Pre-#179.3 this hits the dedup_hit
	// branch and returns without writing — the row stays expired,
	// Recall keeps hiding it. Post-fix, dedupHashes filters the
	// expired row out, the write path proceeds, and the
	// deterministic content-derived ID collides on the same slot —
	// the Upsert REPLACES the expired doc with a fresh one whose
	// expires_at is either unset or a future timestamp.
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I own a labrador named Lucky"}}},
	})
	if err != nil {
		t.Fatalf("Save #2: %v", err)
	}
	if len(second.EntryIDs) == 0 {
		t.Fatalf("second Save returned no IDs; dedup may still be expiry-blind (#179.3 regression)")
	}
	revived, ok, err := idx.Get(ctx, ns, firstID)
	if err != nil || !ok {
		t.Fatalf("post-Save row missing: ok=%v err=%v", ok, err)
	}
	if v, ok := revived.Metadata["expires_at"].(int64); ok && v < frozen.UnixMilli() {
		t.Fatalf("post-Save row still carries in-the-past expires_at=%d (clock=%d); #179.3 fix not effective", v, frozen.UnixMilli())
	}
}

// TestSupersedeOrder_NewDocUpsertedFirst pins issue #167: the
// supersede tagging step must run AFTER the new doc lands so an
// Upsert failure can never leave older entries pointing at an id
// that does not exist.
//
// We assert the *order* indirectly: after a successful Save the
// new entry IS in the index (older slot-mate's MetaSupersededBy
// references a live id, not a dangling one). Coupled with the
// existing TestResolver_UpdateMarksSuperseded assertion that the
// supersede tag still lands, this proves the reorder preserves
// the correctness invariant.
func TestSupersedeOrder_NewDocUpsertedFirst(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Subject: "user", Predicate: "pet_name", Content: "user's pet is named Lucky", Entities: []string{"lucky"}}},
			{{Subject: "user", Predicate: "pet_name", Content: "user's pet is named Max", Entities: []string{"max"}}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := newScope()
	firstIDs := saveText(t, m, scope, "I named my pet Lucky")
	if len(firstIDs) == 0 {
		t.Fatalf("first Save: no entries")
	}
	secondIDs := saveText(t, m, scope, "Actually I named my pet Max")
	if len(secondIDs) == 0 {
		t.Fatalf("second Save: no entries")
	}
	newID := secondIDs[0]
	// The new doc MUST be alive in the index — proves Upsert
	// happened before supersede tagged the old doc.
	if _, ok, err := idx.Get(ctx, recall.NamespaceFor(scope), newID); err != nil || !ok {
		t.Fatalf("new doc %q missing from index; ok=%v err=%v", newID, ok, err)
	}
	// The old doc points at the new id.
	old, ok, err := idx.Get(ctx, recall.NamespaceFor(scope), firstIDs[0])
	if err != nil || !ok {
		t.Fatalf("old doc missing; ok=%v err=%v", ok, err)
	}
	got, _ := old.Metadata[recall.MetaSupersededBy].(string)
	if got != newID {
		t.Fatalf("old doc superseded_by = %q; want %q", got, newID)
	}
}

// TestReconciler_NoBackgroundLoopWhenIntervalNegative checks that
// WithReconcileInterval(-1) really disables the background ticker
// so tests can drive reconciliation deterministically and the
// option behaves the way its godoc claims.
func TestReconciler_NoBackgroundLoopWhenIntervalNegative(t *testing.T) {
	idx := memidx.New()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Just close — the absence of a hanging background goroutine
	// is asserted by Close completing without timing out. (The
	// recall package's Close already Wait()s on reconciler.wg, so
	// any leaked goroutine would block the test forever.)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReconciler_TTLSweepPrunesEntityStore pins issue #151: the
// TTL sweeper hard-deletes expired entries from the primary index
// without notifying the entity store; the Reconciler must drop
// the dead refs on the next sync pass.
func TestReconciler_TTLSweepPrunesEntityStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	past := time.Now().Add(-time.Hour)
	id, err := m.Add(ctx, scope, recall.Entry{
		Content:   "Alice met Bob",
		Entities:  []string{"alice"},
		ExpiresAt: &past, // already expired
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Seed the entity store via a sync (Add does not eager-link;
	// the reconciler is what populates it).
	if err := m.(recall.SideStoreSyncer).SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores #1: %v", err)
	}
	// Sweep deletes the expired doc from primary directly.
	if sw, ok := m.(interface {
		SweepNamespace(context.Context, string) error
	}); ok {
		if err := sw.SweepNamespace(ctx, recall.NamespaceFor(scope)); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	} else {
		t.Skip("Memory does not expose SweepNamespace on this build")
	}
	// Verify primary index dropped the entry.
	if _, ok, _ := idx.Get(ctx, recall.NamespaceFor(scope), id); ok {
		t.Fatalf("primary still has expired entry %q", id)
	}
	// Second sync — the inspector diff must Forget the dead id.
	if err := m.(recall.SideStoreSyncer).SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores #2: %v", err)
	}
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	for _, gid := range got {
		if gid == id {
			t.Fatalf("expired id %q still referenced by entity store post-sweep", id)
		}
	}
}

// TestEntityStoreProjection_AllEntryIDsScansNamespace asserts the
// inspector returns the union of every entity row's linked ids
// for the scope. Used by the Reconciler to compute the stale set
// (retained - alive).
func TestEntityStoreProjection_AllEntryIDsScansNamespace(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"Alice likes coffee","entities":["alice","coffee"]}]`
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	res, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I like Alice"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.EntryIDs) == 0 {
		t.Fatalf("no entries")
	}
	if err := m.(recall.SideStoreSyncer).SyncSideStores(ctx, scope); err != nil {
		t.Fatalf("SyncSideStores: %v", err)
	}
	// After a sync the projection must retain every alive id.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	sort.Strings(got)
	want := append([]string(nil), res.EntryIDs...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("alice retained ids = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("alice retained ids = %v; want %v", got, want)
		}
	}
}
