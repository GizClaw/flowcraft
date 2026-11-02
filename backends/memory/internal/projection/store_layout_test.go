package projection

import (
	"context"
	"path"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// layoutKeys writes one key under one layout version's partition for a scope,
// in the shape that version wrote: a name, then whatever the version kept
// below it. What the sweep gets back is the whole subtree, so the shape below
// the name does not matter to it -- only the partition, which every version
// agrees on.
func layoutKeys(t *testing.T, store *Store[testBase, EntryDelta[testEntry]],
	scope corememory.Scope, version string, keys ...string) []string {
	t.Helper()
	root := path.Join(store.layoutRoot(scope, version), "indexes", "k_index")
	written := make([]string, 0, len(keys))
	for _, key := range keys {
		full := path.Join(root, key)
		if err := store.KV.Put(context.Background(), full, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		written = append(written, full)
	}
	return written
}

func keysRemaining(t *testing.T, store *Store[testBase, EntryDelta[testEntry]],
	scope corememory.Scope, version string) int {
	t.Helper()
	keys, err := indexKeys(context.Background(), store.KV, store.layoutRoot(scope, version))
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

// TestReclaimLegacyLayoutsDropsTheTreeNoBuildReads pins the whole of the sweep:
// the layout this build does not write is gone, key for key, and the one it
// does write is untouched -- including the state a manifest names, which the
// read path resolves.
func TestReclaimLegacyLayoutsDropsTheTreeNoBuildReads(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 2, MinDeltaBytes: 1 << 20})
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	layoutKeys(t, store, scope, "v2", "bases/k_a.json", "bases/k_b.json", "segments/k_c.json")
	current := layoutKeys(t, store, scope, layoutVersion, "indexes-name/bases/k_d.json")

	result, err := store.ReclaimLegacyLayouts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Keys != 3 {
		t.Fatalf("swept %d keys, want the three the v2 layout held: %+v", result.Keys, result)
	}
	if len(result.Layouts) != 1 || result.Layouts[0].Version != "v2" || result.Layouts[0].Keys != 3 {
		t.Fatalf("layouts = %+v, want one v2 layout of three keys", result.Layouts)
	}
	if remaining := keysRemaining(t, store, scope, "v2"); remaining != 0 {
		t.Fatalf("v2 layout still holds %d keys", remaining)
	}
	if remaining := keysRemaining(t, store, scope, layoutVersion); remaining != 1 {
		t.Fatalf("current layout holds %d keys, want the one this build wrote", remaining)
	}
	for _, key := range current {
		if _, err := store.KV.Get(ctx, key); err != nil {
			t.Fatalf("the sweep removed a key of the layout this build writes: %v", err)
		}
	}
}

// TestReclaimLegacyLayoutsStopsAtThePartition is the tenant boundary: the
// partition is the only address the two layouts agree on, so it is also the
// only thing keeping one host's sweep out of the scope beside it.
func TestReclaimLegacyLayoutsStopsAtThePartition(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 2, MinDeltaBytes: 1 << 20})
	swept := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	neighbour := corememory.Scope{RuntimeID: "runtime", UserID: "user-2"}
	layoutKeys(t, store, swept, "v2", "bases/k_a.json")
	theirs := layoutKeys(t, store, neighbour, "v2", "bases/k_a.json")

	if _, err := store.ReclaimLegacyLayouts(ctx, swept); err != nil {
		t.Fatal(err)
	}
	for _, key := range theirs {
		if _, err := store.KV.Get(ctx, key); err != nil {
			t.Fatalf("the sweep reached another scope: %v", err)
		}
	}
}

// TestReclaimLegacyLayoutsIsIdempotent pins the retry: a second sweep finds
// nothing, which is what makes a failed one safe to run again.
func TestReclaimLegacyLayoutsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 2, MinDeltaBytes: 1 << 20})
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	layoutKeys(t, store, scope, "v2", "bases/k_a.json")

	if _, err := store.ReclaimLegacyLayouts(ctx, scope); err != nil {
		t.Fatal(err)
	}
	again, err := store.ReclaimLegacyLayouts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() || len(again.Layouts) != 0 {
		t.Fatalf("second sweep = %+v, want nothing left", again)
	}
}

// TestLegacyLayoutsDoNotNameTheCurrentLayout is the guard on the list itself: a
// version that appears in both is refused before a single delete, because that
// list would sweep the state every read resolves.
func TestLegacyLayoutsDoNotNameTheCurrentLayout(t *testing.T) {
	for _, version := range legacyLayoutVersions {
		if version == layoutVersion {
			t.Fatalf("legacy layout list names the current layout %q", version)
		}
		if strings.TrimSpace(version) != version || version == "" {
			t.Fatalf("legacy layout %q is not a path segment", version)
		}
	}
}
