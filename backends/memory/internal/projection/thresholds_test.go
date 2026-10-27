package projection

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestThresholdsTriggerOnTheShareOfBaseTheDeltasReach pins the trigger itself:
// the segment cap is an upper bound on what a manifest describes, the floor
// keeps a small base from being rewritten on every commit, and anything in
// between compacts when the deltas reach their share of the base.
func TestThresholdsTriggerOnTheShareOfBaseTheDeltasReach(t *testing.T) {
	thresholds := Thresholds{MaxSegments: 64, MinDeltaBytes: 4 << 20, Ratio: 2}
	for _, test := range []struct {
		name       string
		segments   int
		deltaBytes int64
		baseBytes  int64
		want       bool
	}{
		{name: "empty base under the floor", segments: 1, deltaBytes: 4 << 20, baseBytes: 0},
		{name: "empty base over the floor", segments: 1, deltaBytes: 5 << 20, baseBytes: 0, want: true},
		{name: "large base under its share", segments: 1, deltaBytes: 40 << 20, baseBytes: 100 << 20},
		{name: "large base over its share", segments: 1, deltaBytes: 51 << 20, baseBytes: 100 << 20, want: true},
		{name: "segment cap", segments: 65, deltaBytes: 1, baseBytes: 100 << 20, want: true},
		{name: "segment cap is inclusive", segments: 64, deltaBytes: 1, baseBytes: 100 << 20},
	} {
		if got := thresholds.over(test.segments, test.deltaBytes, test.baseBytes); got != test.want {
			t.Errorf("%s: over(%d, %d, %d) = %v, want %v",
				test.name, test.segments, test.deltaBytes, test.baseBytes, got, test.want)
		}
	}
	// A ratio of one makes the whole base the trigger, and nothing else.
	aggressive := Thresholds{MaxSegments: 64, MinDeltaBytes: 1, Ratio: 1}
	if aggressive.over(1, 1<<20, 8<<20) {
		t.Error("a delta of one eighth of the base compacted under a ratio of one")
	}
	if !aggressive.over(1, 9<<20, 8<<20) {
		t.Error("a delta larger than the base did not compact under a ratio of one")
	}
}

func TestThresholdsNormalizeDefaultsAndRejectNonPositiveValues(t *testing.T) {
	normalized, err := Thresholds{}.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if normalized != DefaultThresholds() {
		t.Fatalf("normalized = %+v, want %+v", normalized, DefaultThresholds())
	}
	for _, thresholds := range []Thresholds{
		{MaxSegments: -1},
		{MinDeltaBytes: -1},
		{Ratio: -1},
	} {
		if _, err := thresholds.normalized(); err == nil {
			t.Errorf("%+v normalized without an error", thresholds)
		}
	}
}

// TestTypedStoreCompactsOnTheShareItsDeltasReach is the wiring: with a base of
// known size and a byte floor small enough to be irrelevant, the store keeps
// appending segments until the deltas reach their share of the base, and only
// then rewrites it. The rule this replaced compacted the first delta of every
// base, and on a scope-sized base it did so every 32 commits.
func TestTypedStoreCompactsOnTheShareItsDeltasReach(t *testing.T) {
	store := newTestStore(t, kvFor(t), Thresholds{MaxSegments: 4096, MinDeltaBytes: 1, Ratio: 2})
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	base := testBase{Entries: make([]testEntry, 400)}
	for index := range base.Entries {
		base.Entries[index] = testEntry{ID: fmt.Sprintf("entry-%03d", index), Value: strings.Repeat("x", 64)}
	}
	if err := store.FullRebuild(context.Background(), scope, "index", base, "source-base"); err != nil {
		t.Fatal(err)
	}
	_, manifest, err := store.Materialize(context.Background(), scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	baseBytes := manifest.Base.Bytes
	share := baseBytes / 2

	var stored int64
	applied := 0
	for ; applied < 10000; applied++ {
		result, err := store.ApplyDelta(context.Background(), scope, "index",
			EntryDelta[testEntry]{Upserts: []testEntry{{
				ID: fmt.Sprintf("added-%05d", applied), Value: strings.Repeat("y", 64),
			}}}, fmt.Sprintf("revision-%d", applied), fmt.Sprintf("source-%d", applied))
		if err != nil {
			t.Fatal(err)
		}
		if result.Compacted {
			break
		}
		stored = 0
		for _, ref := range result.Manifest.Segments {
			stored += ref.Bytes
		}
	}
	if applied == 10000 {
		t.Fatal("the store never compacted")
	}
	if applied < 2 {
		t.Fatalf("compacted after %d deltas over a %d byte base", applied, baseBytes)
	}
	// The segment that triggered the compaction is added to the stored bytes
	// before the trigger is evaluated, so what has to hold is that the last
	// observation before the rewrite was already a meaningful share of the base.
	if stored < share/2 {
		t.Fatalf("compacted after %d bytes of deltas over a %d byte base (share %d)",
			stored, baseBytes, share)
	}
	if stored > share+stored/2 {
		t.Fatalf("compacted after %d bytes of deltas, well past the %d byte share", stored, share)
	}
}
