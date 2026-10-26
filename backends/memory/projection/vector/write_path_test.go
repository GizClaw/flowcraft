package vector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// recordingStore counts the reads of stored state -- a lane's bases and
// segments -- and can hold the first of them open for a while, so a test can
// tell a table the lane decoded again from one it already had, and can keep a
// background load running while it writes.
type recordingStore struct {
	storage.Store
	delay time.Duration
	once  sync.Once
	mu    sync.Mutex
	reads int
}

func (recorder *recordingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/bases/") || strings.Contains(key, "/segments/") {
		recorder.mu.Lock()
		recorder.reads++
		recorder.mu.Unlock()
		if recorder.delay > 0 {
			recorder.once.Do(func() { time.Sleep(recorder.delay) })
		}
	}
	return recorder.Store.Get(ctx, key)
}

func (recorder *recordingStore) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	put, ok := recorder.Store.(storage.PutIfAbsentStore)
	if !ok {
		return false, nil
	}
	return put.PutIfAbsent(ctx, key, data)
}

func (recorder *recordingStore) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.reads
}

func recordingKVFor(t *testing.T, delay time.Duration) *recordingStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return &recordingStore{Store: kvStore, delay: delay}
}

// TestDeltaAppliesShareOneDecodedBase is the derivation's cost pinned: a pass
// applies one delta per commit, and each apply reads the entries the base
// already holds, to embed only what changed and to reuse the vector of what did
// not. Decoding the table per apply made a derivation quadratic -- 14.0s per
// materialization of the eval's 9324-entry table, hundreds of applies per
// scope -- so the deltas after the first share the base the store kept, and the
// only read left is the one that fills it.
func TestDeltaAppliesShareOneDecodedBase(t *testing.T) {
	ctx := context.Background()
	kvStore := recordingKVFor(t, 0)
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvStore, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	const deltas = 8
	for number := 0; number < deltas; number++ {
		delta := component.ProjectionDelta{
			Scope: scope, Projection: "facts",
			Upserts: []component.Artifact{vectorArtifact(fmt.Sprintf("a-%d", number), "alpha")},
		}
		if err := index.ApplyDelta(ctx, delta); err != nil {
			t.Fatal(err)
		}
	}
	if reads := kvStore.count(); reads > 1 {
		t.Fatalf("%d deltas read the stored state %d times, want at most 1", deltas, reads)
	}
	// What the shared base produced is what a fresh reader of the same store
	// materializes: the lane reused nothing it did not already hold.
	fresh, err := New(Config{KV: kvStore, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := fresh.SearchVector(ctx, component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0}, Limit: deltas * 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != deltas {
		t.Fatalf("the store holds %d entries, want %d", len(results), deltas)
	}
}

// TestWarmUpIsCoalescedPerScope pins the other half of the write path's cost: a
// warm-up per write, when a derivation writes once per commit, is a run of
// loads standing in for each other. One load runs at a time, and the writes
// that arrive during it ask for one more rather than one each.
func TestWarmUpIsCoalescedPerScope(t *testing.T) {
	ctx := context.Background()
	kvStore := recordingKVFor(t, 150*time.Millisecond)
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvStore, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(ctx, component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	// The rebuild's warm-up is the load the delay holds open: every ask below
	// lands while it runs, and one of them is the load it owes.
	for ask := 0; ask < 5; ask++ {
		index.warmSnapshots(ctx, scope)
	}
	waitForStats(t, index, "the coalesced warm-ups settling", func(stats Stats) bool {
		return stats.WarmHits+stats.WarmMisses+stats.WarmErrors >= 2
	})
	settled := index.Stats()
	if loads := settled.WarmHits + settled.WarmMisses + settled.WarmErrors; loads != 2 {
		t.Fatalf("six asks ran %d loads, want 2", loads)
	}
	// The partition is free again: the next write warms it on its own.
	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Projection: "facts",
		Upserts: []component.Artifact{vectorArtifact("b", "alpha again")},
	}); err != nil {
		t.Fatal(err)
	}
	waitForStats(t, index, "the write after the coalesced run warming its scope", func(stats Stats) bool {
		return stats.WarmHits+stats.WarmMisses+stats.WarmErrors >= 3
	})
}
