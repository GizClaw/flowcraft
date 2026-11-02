package projection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// materializeStateError reads one state and returns the StateError it must
// report, failing the test when the read reports anything else.
func materializeStateError(t *testing.T, store *Store[testBase, EntryDelta[testEntry]], scope corememory.Scope) *component.StateError {
	t.Helper()
	_, _, err := store.Materialize(context.Background(), testAddress(scope), "index")
	if err == nil {
		t.Fatal("state that cannot be read materialized without an error")
	}
	var state *component.StateError
	if !errors.As(err, &state) {
		t.Fatalf("err = %v, want a StateError", err)
	}
	if state.Projection != "index" {
		t.Fatalf("state error names %q", state.Projection)
	}
	return state
}

// A scope that holds no state and a scope whose state cannot be read are two
// different answers, and the store is where they part: the first is NotFound,
// which a reader reports as "never written here", the second is a StateError,
// which says the manifest exists and what it points at does not read back.
func TestMaterializeSeparatesUnreadableStateFromNoState(t *testing.T) {
	ctx := context.Background()
	kvStore := kvFor(t)
	store := newTestStore(t, kvStore, DefaultThresholds())
	scope := corememory.Scope{RuntimeID: "runtime"}

	if _, _, err := store.Materialize(ctx, testAddress(scope), "index"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("empty scope err = %v, want NotFound", err)
	}

	if err := store.FullRebuild(ctx, testAddress(scope), "index",
		testBase{Entries: []testEntry{{ID: "a", Value: "value"}}}, "source"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Materialize(ctx, testAddress(scope), "index"); err != nil {
		t.Fatalf("readable state err = %v", err)
	}
	manifest, found, err := store.loadManifestLocked(ctx, testAddress(scope), "index")
	if err != nil || !found || manifest.Base == nil {
		t.Fatalf("manifest = %+v, found = %v, err = %v", manifest, found, err)
	}

	payload, err := kvStore.Get(ctx, store.buildPath(testAddress(scope), "index", manifest.Base.ID))
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)/2] ^= 0x01
	if err := kvStore.Put(ctx, store.buildPath(testAddress(scope), "index", manifest.Base.ID), payload); err != nil {
		t.Fatal(err)
	}
	// The read is a fresh store's: the store that wrote the state holds it
	// materialized, so only a reader without that cache meets the payload.
	state := materializeStateError(t, newTestStore(t, kvStore, DefaultThresholds()), scope)
	if errors.Is(state, storage.ErrNotFound) {
		t.Fatalf("unreadable state reports NotFound: %v", state)
	}
	if !strings.Contains(state.Error(), "base digest mismatch") {
		t.Fatalf("state error does not say what failed: %v", state)
	}

	// The manifest area is the same answer: a manifest that no longer decodes
	// is state that exists and cannot be served, not a scope that was never
	// written to.
	if err := kvStore.Put(ctx, store.activePath(testAddress(scope), "index"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	if state := materializeStateError(t, newTestStore(t, kvStore, DefaultThresholds()), scope); !strings.Contains(state.Error(), "active manifest") {
		t.Fatalf("state error does not say what failed: %v", state)
	}
}

// TestMaterializeReportsASegmentDeltaThatDoesNotRecompute reproduces the
// condition issue #651 was opened on: a store whose manifest is intact and
// whose segment decodes cleanly, but whose decoded delta no longer encodes to
// the digest the segment records. Forging it takes rewriting the segment
// payload and the manifest's reference together -- the payload re-encoded, its
// own digest updated, the delta digest left as it was -- which is exactly the
// state a reader meets when the two disagree. The error has to name both
// digests: everything else about the segment agrees with the manifest, so they
// are what a host can compare against a rebuilt lane.
func TestMaterializeReportsASegmentDeltaThatDoesNotRecompute(t *testing.T) {
	ctx := context.Background()
	kvStore := kvFor(t)
	store := newTestStore(t, kvStore, DefaultThresholds())
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, testAddress(scope), "index", testBase{}, "source"); err != nil {
		t.Fatal(err)
	}
	applied, err := store.ApplyDelta(ctx, testAddress(scope), "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "a", Value: "value"}}}, "revision", "input")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Manifest.Segments) != 1 {
		t.Fatalf("segments = %+v", applied.Manifest.Segments)
	}
	ref := applied.Manifest.Segments[0]
	payload, err := kvStore.Get(ctx, store.segmentPath(testAddress(scope), "index", ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	var item segment[EntryDelta[testEntry]]
	if err := Decode(payload, &item); err != nil {
		t.Fatal(err)
	}
	item.Delta.Upserts[0].Value = "rewritten"
	rewritten, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := kvStore.Put(ctx, store.segmentPath(testAddress(scope), "index", ref.ID), rewritten); err != nil {
		t.Fatal(err)
	}
	manifest := applied.Manifest
	manifest.Segments[0].Bytes = int64(len(rewritten))
	manifest.Segments[0].Digest = digestBytes(rewritten)
	manifest.Identity = manifestIdentity(manifest)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := kvStore.Put(ctx, store.activePath(testAddress(scope), "index"), encoded); err != nil {
		t.Fatal(err)
	}
	recomputed := digestBytes(func() []byte {
		data, marshalErr := json.Marshal(item.Delta)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return data
	}())
	if recomputed == ref.DeltaDigest {
		t.Fatal("the fixture did not make the delta recompute differently")
	}

	state := materializeStateError(t, store, scope)
	if !strings.Contains(state.Error(), "segment 0 delta digest mismatch") {
		t.Fatalf("state error = %v", state)
	}
	for _, digest := range []string{ref.DeltaDigest, recomputed} {
		if !strings.Contains(state.Error(), digest) {
			t.Fatalf("state error does not name digest %s: %v", digest, state)
		}
	}
}
