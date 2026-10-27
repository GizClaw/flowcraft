package projection

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// countingStore counts the reads of stored state -- the bases and the segments
// -- so a test can tell a base the store decoded again from a base it already
// held. The manifest is not counted: every call reads it to learn which state
// is active.
type countingStore struct {
	storage.Store
	mu    sync.Mutex
	reads int
}

func (counter *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/bases/") || strings.Contains(key, "/segments/") {
		counter.mu.Lock()
		counter.reads++
		counter.mu.Unlock()
	}
	return counter.Store.Get(ctx, key)
}

func (counter *countingStore) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	put, ok := counter.Store.(storage.PutIfAbsentStore)
	if !ok {
		return false, nil
	}
	return put.PutIfAbsent(ctx, key, data)
}

func (counter *countingStore) count() int {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.reads
}

func countingKVFor(t *testing.T) *countingStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return &countingStore{Store: kvStore}
}

// TestTypedStoreKeepsItsBaseAcrossItsOwnDeltas is the write path's own
// regression: a derivation applies one delta per commit, and every apply reads
// the entries the base already holds to reuse what it can. Re-decoding the base
// per apply is what made the eval's derivation quadratic -- 14.0s to
// materialize one 9324-entry table, hundreds of times per scope -- so the store
// keeps the base it decoded and moves it forward with the deltas it publishes.
func TestTypedStoreKeepsItsBaseAcrossItsOwnDeltas(t *testing.T) {
	kvStore := countingKVFor(t)
	store := newTestStore(t, kvStore, Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20})
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Materialize(ctx, scope, "index"); err != nil {
		t.Fatal(err)
	}
	settled := kvStore.count()
	for index := 0; index < 8; index++ {
		delta := EntryDelta[testEntry]{Upserts: []testEntry{{ID: string(rune('a' + index)), Value: "value"}}}
		if _, err := store.ApplyDelta(ctx, scope, "index", delta, "revision", "input"); err != nil {
			t.Fatal(err)
		}
	}
	if reads := kvStore.count() - settled; reads != 0 {
		t.Fatalf("eight deltas read the stored state %d times, want 0", reads)
	}
	// What the store advanced in memory is the state a reader decodes.
	restarted := newTestStore(t, kvStore, Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20})
	replayed, _, err := restarted.Materialize(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	advanced, _, err := store.MaterializeView(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if len(advanced.Entries) != 8 || len(replayed.Entries) != 8 {
		t.Fatalf("advanced %d entries, replayed %d, want 8", len(advanced.Entries), len(replayed.Entries))
	}
	for index := range replayed.Entries {
		if advanced.Entries[index] != replayed.Entries[index] {
			t.Fatalf("entry %d advanced to %+v, replay says %+v",
				index, advanced.Entries[index], replayed.Entries[index])
		}
	}
}

// TestTypedStoreViewDoesNotMoveWithTheState pins what a view is: the state the
// caller read, handed over without a copy. A delta that follows it replaces the
// store's slices rather than rewriting them, so the view a caller already holds
// keeps describing what it read while the next view describes the new state.
func TestTypedStoreViewDoesNotMoveWithTheState(t *testing.T) {
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20})
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, scope, "index",
		testBase{Entries: []testEntry{{ID: "a", Value: "one"}}}, "base"); err != nil {
		t.Fatal(err)
	}
	view, _, err := store.MaterializeView(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyDelta(ctx, scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "b", Value: "two"}}}, "revision", "input"); err != nil {
		t.Fatal(err)
	}
	if len(view.Entries) != 1 {
		t.Fatalf("the view moved with the write: %+v", view.Entries)
	}
	next, _, err := store.MaterializeView(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Entries) != 2 {
		t.Fatalf("the next view holds %d entries, want 2", len(next.Entries))
	}
	// A caller that owns its base is still handed one: writing through it
	// cannot reach the store's copy.
	owned, _, err := store.Materialize(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	owned.Entries[0].Value = "corrupted"
	again, _, err := store.MaterializeView(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if again.Entries[0].Value == "corrupted" {
		t.Fatal("a caller's copy is the store's copy")
	}
}

// TestTypedStoreResidentBoundEvictsTheColdBase pins what the bound costs: the
// partition it evicted is decoded again, never answered differently.
func TestTypedStoreResidentBoundEvictsTheColdBase(t *testing.T) {
	kvStore := countingKVFor(t)
	store := newTestStoreWith(t, kvStore, Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20}, 1)
	ctx := context.Background()
	first := corememory.Scope{RuntimeID: "first"}
	second := corememory.Scope{RuntimeID: "second"}
	for _, scope := range []corememory.Scope{first, second} {
		if err := store.FullRebuild(ctx, scope, "index",
			testBase{Entries: []testEntry{{ID: "a", Value: "one"}}}, "base"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.MaterializeView(ctx, first, "index"); err != nil {
		t.Fatal(err)
	}
	settled := kvStore.count()
	if _, _, err := store.MaterializeView(ctx, first, "index"); err != nil {
		t.Fatal(err)
	}
	if reads := kvStore.count() - settled; reads != 0 {
		t.Fatalf("the resident base was decoded again: %d reads", reads)
	}
	if _, _, err := store.MaterializeView(ctx, second, "index"); err != nil {
		t.Fatal(err)
	}
	settled = kvStore.count()
	if _, _, err := store.MaterializeView(ctx, first, "index"); err != nil {
		t.Fatal(err)
	}
	if reads := kvStore.count() - settled; reads == 0 {
		t.Fatal("a base the bound evicted was served from memory")
	}
}

// TestTypedStoreDoesNotAdvanceABaseAnotherWriterReplaced pins the identity
// check the advance relies on: the base the store keeps is only moved forward
// when it is the state the delta was applied to. A writer this store did not
// see leaves the manifest naming other state, and the store decodes that state
// instead of advancing a base that no longer describes it.
func TestTypedStoreDoesNotAdvanceABaseAnotherWriterReplaced(t *testing.T) {
	kvStore := kvFor(t)
	thresholds := Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20}
	mine := newTestStore(t, kvStore, thresholds)
	theirs := newTestStore(t, kvStore, thresholds)
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := mine.FullRebuild(ctx, scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mine.MaterializeView(ctx, scope, "index"); err != nil {
		t.Fatal(err)
	}
	if _, err := theirs.ApplyDelta(ctx, scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "theirs", Value: "theirs"}}}, "revision", "input"); err != nil {
		t.Fatal(err)
	}
	if _, err := mine.ApplyDelta(ctx, scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "mine", Value: "mine"}}}, "revision", "input"); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := newTestStore(t, kvStore, thresholds).Materialize(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	advanced, _, err := mine.MaterializeView(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if len(advanced.Entries) != len(replayed.Entries) {
		t.Fatalf("advanced %d entries, replay says %d: %+v",
			len(advanced.Entries), len(replayed.Entries), advanced.Entries)
	}
	for index := range replayed.Entries {
		if advanced.Entries[index] != replayed.Entries[index] {
			t.Fatalf("entry %d advanced to %+v, replay says %+v",
				index, advanced.Entries[index], replayed.Entries[index])
		}
	}
}
