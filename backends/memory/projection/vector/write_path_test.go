package vector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// recordingStore counts the reads of stored state -- a lane's bases and
// segments -- so a test can tell a table the store decoded again from one it
// already held.
type recordingStore struct {
	storage.Store
	mu    sync.Mutex
	reads int
}

func (recorder *recordingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/bases/") || strings.Contains(key, "/segments/") {
		recorder.mu.Lock()
		recorder.reads++
		recorder.mu.Unlock()
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

func recordingKVFor(t *testing.T) *recordingStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return &recordingStore{Store: kvStore}
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
	kvStore := recordingKVFor(t)
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvStore, Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	const deltas = 8
	for number := 0; number < deltas; number++ {
		delta := component.ProjectionDelta{
			Scope: scope, Shard: shardOf("conversation"), Projection: "facts",
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
