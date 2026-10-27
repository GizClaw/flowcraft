package projection

import (
	"context"
	"path"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// storedObjects counts the bases and segments one projection holds on disk,
// whatever the manifest names. It is what says whether a sweep ran at all: the
// manifest cannot, because it only ever describes what is current.
func storedObjects(t *testing.T, store *Store[testBase, EntryDelta[testEntry]],
	scope corememory.Scope, name string) (bases, segments int) {
	t.Helper()
	root := store.shardRoot(testAddress(scope), name)
	keys, err := indexKeys(context.Background(), store.KV, root)
	if err != nil {
		t.Fatal(err)
	}
	basesPrefix := path.Join(root, "bases") + "/"
	segmentsPrefix := path.Join(root, "segments") + "/"
	for _, key := range keys {
		switch {
		case strings.HasPrefix(key, basesPrefix):
			bases++
		case strings.HasPrefix(key, segmentsPrefix):
			segments++
		}
	}
	return bases, segments
}

func assertStoredObjects(t *testing.T, store *Store[testBase, EntryDelta[testEntry]],
	scope corememory.Scope, name string, wantBases, wantSegments int) {
	t.Helper()
	bases, segments := storedObjects(t, store, scope, name)
	if bases != wantBases || segments != wantSegments {
		t.Fatalf("stored objects = %d bases, %d segments, want %d and %d", bases, segments, wantBases, wantSegments)
	}
}

// TestTypedStorePublicationKeepsOneSupersededGeneration pins the bound the
// publication-time sweep exists for: a compaction folds the state it read into
// a new base, so the state it replaced is unreachable the moment the new
// manifest lands, and the sweep keeps exactly that one generation -- enough for
// a reader that resolved the previous manifest to finish -- and drops it when
// the next publication replaces that.
func TestTypedStorePublicationKeepsOneSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 2, MinDeltaBytes: 1 << 20})
	scope := corememory.Scope{RuntimeID: "runtime"}
	upsert := func(id, revision string) error {
		_, err := store.ApplyDelta(ctx, testAddress(scope), "index",
			EntryDelta[testEntry]{Upserts: []testEntry{{ID: id, Value: "value"}}}, revision, revision)
		return err
	}
	if err := store.FullRebuild(ctx, testAddress(scope), "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	// The first publication replaces nothing, so there is nothing to keep and
	// nothing to drop.
	assertStoredObjects(t, store, scope, "index", 1, 0)

	// Two deltas fit under the threshold: both are named by the manifest.
	for _, id := range []string{"a", "b"} {
		if err := upsert(id, "revision-"+id); err != nil {
			t.Fatal(err)
		}
	}
	assertStoredObjects(t, store, scope, "index", 1, 2)

	// The third compacts them into a new base. The base and the segments it
	// folded are the generation this publication replaced.
	if err := upsert("c", "revision-c"); err != nil {
		t.Fatal(err)
	}
	assertStoredObjects(t, store, scope, "index", 2, 2)

	// A delta on top of the new base is named; the replaced generation is the
	// grace it was kept as.
	if err := upsert("d", "revision-d"); err != nil {
		t.Fatal(err)
	}
	assertStoredObjects(t, store, scope, "index", 2, 3)

	if err := upsert("e", "revision-e"); err != nil {
		t.Fatal(err)
	}
	assertStoredObjects(t, store, scope, "index", 2, 4)

	// The next compaction is what drops the generation two publications back:
	// the lane holds two generations, never the whole history.
	if err := upsert("f", "revision-f"); err != nil {
		t.Fatal(err)
	}
	assertStoredObjects(t, store, scope, "index", 2, 2)

	// What the sweep removed was storage, not state: the lane still reads back
	// every entry the manifests named along the way.
	got, _, err := store.Materialize(ctx, testAddress(scope), "index")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 6 {
		t.Fatalf("materialized = %d entries, want 6", len(got.Entries))
	}
}

// TestReclaimKeepsNothingButTheActiveManifest is the host entry point: a
// workspace an earlier build filled holds a generation of grace (and, before
// this sweep existed, every generation it ever published), and one call leaves
// exactly what the active manifest names.
func TestReclaimKeepsNothingButTheActiveManifest(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 2, MinDeltaBytes: 1 << 20})
	scope := corememory.Scope{RuntimeID: "runtime"}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, err := store.ApplyDelta(ctx, testAddress(scope), "index",
			EntryDelta[testEntry]{Upserts: []testEntry{{ID: id, Value: "value"}}}, "revision-"+id, "revision-"+id); err != nil {
			t.Fatal(err)
		}
	}
	// The lane's whole history as an earlier build left it: the sweep below is
	// the only thing that ever removes it.
	if bases, _ := storedObjects(t, store, scope, "index"); bases != 2 {
		t.Fatalf("stored bases before the sweep = %d, want the publication's two", bases)
	}

	reclaimed, err := store.Reclaim(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Empty() || reclaimed.Bases != 1 || reclaimed.Segments != 2 {
		t.Fatalf("reclaimed = %+v, want the superseded base and segment", reclaimed)
	}
	assertStoredObjects(t, store, scope, "index", 1, 0)

	// A sweep that finds nothing is the normal repeated case, not an error.
	again, err := store.Reclaim(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("second sweep reclaimed %+v", again)
	}
	got, _, err := store.Materialize(ctx, testAddress(scope), "index")
	if err != nil || len(got.Entries) != 6 {
		t.Fatalf("materialized after the sweep = %d entries, %v", len(got.Entries), err)
	}
}

// TestReclaimCollectsABaseNoManifestNames is the other shape of the leak: a
// publication that wrote its base and failed before moving the manifest left
// storage nothing points at, and no publication after it will ever name the
// object again. The sweep collects it like any other unreachable base.
func TestReclaimCollectsABaseNoManifestNames(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), DefaultThresholds())
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, testAddress(scope), "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	orphan := store.buildPath(testAddress(scope), "index", strings.Repeat("0", 64))
	if err := store.writeImmutable(ctx, orphan, []byte(`{"entries":[]}`)); err != nil {
		t.Fatal(err)
	}
	assertStoredObjects(t, store, scope, "index", 2, 0)

	reclaimed, err := store.Reclaim(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Bases != 1 || reclaimed.Segments != 0 {
		t.Fatalf("reclaimed = %+v, want the orphaned base alone", reclaimed)
	}
	assertStoredObjects(t, store, scope, "index", 1, 0)
}

// TestReclaimRefusesAManifestItCannotRead pins the safety property: the sweep's
// question is what the active manifest names, and a manifest it cannot decode
// has no answer. Refusing is what keeps a corrupt pointer from deleting the
// state a repair would have read.
func TestReclaimRefusesAManifestItCannotRead(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 1, MinDeltaBytes: 1 << 20})
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, testAddress(scope), "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := store.ApplyDelta(ctx, testAddress(scope), "index",
			EntryDelta[testEntry]{Upserts: []testEntry{{ID: id, Value: "value"}}}, "revision-"+id, "revision-"+id); err != nil {
			t.Fatal(err)
		}
	}
	assertStoredObjects(t, store, scope, "index", 2, 1)
	if err := store.KV.Put(ctx, store.activePath(testAddress(scope), "index"), []byte(`{"schema_version":`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reclaim(ctx, scope, "index"); err == nil {
		t.Fatal("the sweep accepted a manifest it cannot read")
	}
	assertStoredObjects(t, store, scope, "index", 2, 1)
}

// TestReclaimLeavesOtherNamesAndForeignKeysAlone pins the two boundaries of the
// sweep: another name of the same family is another projection (the vector
// lane's generations are named after the model), and a key that is not a base
// or a segment was not written by this protocol. Neither is this sweep's to
// remove.
func TestReclaimLeavesOtherNamesAndForeignKeysAlone(t *testing.T) {
	ctx := context.Background()
	kvStore := kvFor(t)
	store := newTestStore(t, kvStore, DefaultThresholds())
	scope := corememory.Scope{RuntimeID: "runtime"}
	for _, name := range []string{"index", "generation-older"} {
		if err := store.FullRebuild(ctx, testAddress(scope), name, testBase{}, "base-"+name); err != nil {
			t.Fatal(err)
		}
	}
	foreign := path.Join(store.root(scope, "index"), "scratch", "marker")
	if err := kvStore.Put(ctx, foreign, []byte("kept")); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := store.Reclaim(ctx, scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed.Empty() {
		t.Fatalf("reclaimed = %+v, want nothing reachable to remove", reclaimed)
	}
	assertStoredObjects(t, store, scope, "generation-older", 1, 0)
	if _, err := kvStore.Get(ctx, foreign); err != nil {
		t.Fatalf("the sweep removed a key it does not own: %v", err)
	}

	// A name that was never written holds nothing, which is not a reason to
	// fail: a host sweeping a fresh scope sweeps every lane.
	fresh, err := store.Reclaim(ctx, scope, "never-written")
	if err != nil || !fresh.Empty() {
		t.Fatalf("sweep of a name that was never written = %+v, %v", fresh, err)
	}
}
