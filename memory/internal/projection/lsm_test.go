package projection

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type testEntry struct {
	ID         string `json:"id"`
	DatasetID  string `json:"dataset_id,omitempty"`
	DocumentID string `json:"document_id,omitempty"`
	Value      string `json:"value"`
}

type testBase struct {
	Entries []testEntry `json:"entries"`
}

func newTestStore(t *testing.T, ws workspace.Workspace, thresholds Thresholds) *Store[testBase, EntryDelta[testEntry]] {
	t.Helper()
	store, err := NewTypedStore(ws, "test", TypedOptions[testBase, EntryDelta[testEntry]]{
		Thresholds: thresholds,
		Canonicalize: func(delta EntryDelta[testEntry]) EntryDelta[testEntry] {
			return CanonicalEntryDelta(delta, EntryKey[testEntry]{
				ID: func(value testEntry) string { return value.ID },
				Document: func(value testEntry) component.DocumentAddress {
					return component.DocumentAddress{DatasetID: value.DatasetID, DocumentID: value.DocumentID}
				},
			})
		},
		Apply: func(base *testBase, delta EntryDelta[testEntry]) error {
			base.Entries = ApplyEntryDelta(base.Entries, delta, EntryKey[testEntry]{
				ID: func(value testEntry) string { return value.ID },
				Document: func(value testEntry) component.DocumentAddress {
					return component.DocumentAddress{DatasetID: value.DatasetID, DocumentID: value.DocumentID}
				},
			})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTypedStoreDeltaPublishIsSmallReplayableAndOrdered(t *testing.T) {
	meter := &meterWorkspace{Workspace: workspace.NewMemWorkspace()}
	store := newTestStore(t, meter, Thresholds{MaxSegments: 64, MaxDeltaBytes: 1 << 20})
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	base := testBase{Entries: make([]testEntry, 1000)}
	for index := range base.Entries {
		base.Entries[index] = testEntry{ID: string(rune(index + 1)), Value: strings.Repeat("x", 64)}
	}
	if err := store.FullRebuild(context.Background(), scope, "index", base, "source-base"); err != nil {
		t.Fatal(err)
	}
	meter.reset()
	delta := EntryDelta[testEntry]{Upserts: []testEntry{{ID: "changed", Value: "small"}}}
	first, err := store.ApplyDelta(context.Background(), scope, "index", delta, "revision-1", "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if meter.bytes > 4096 {
		t.Fatalf("single delta wrote %d bytes", meter.bytes)
	}
	replayed, err := store.ApplyDelta(context.Background(), scope, "index", delta, "revision-1", "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Manifest.Identity != first.Manifest.Identity {
		t.Fatalf("replay = %+v, first = %+v", replayed, first)
	}
	if _, err := store.ApplyDelta(context.Background(), scope, "index",
		EntryDelta[testEntry]{DeleteIDs: []string{"changed"}}, "revision-2", "source-2"); err != nil {
		t.Fatal(err)
	}
	materialized, manifest, err := store.Materialize(context.Background(), scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized.Entries) != 1000 || len(manifest.Segments) != 2 {
		t.Fatalf("entries=%d segments=%d", len(materialized.Entries), len(manifest.Segments))
	}
}

func TestTypedStoreCrashBeforeManifestLeavesInvisibleReusableOrphan(t *testing.T) {
	base := workspace.NewMemWorkspace()
	fault := &manifestFaultWorkspace{Workspace: base}
	store := newTestStore(t, fault, DefaultThresholds())
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(context.Background(), scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	fault.failManifest = true
	delta := EntryDelta[testEntry]{Upserts: []testEntry{{ID: "a", Value: "new"}}}
	if _, err := store.ApplyDelta(context.Background(), scope, "index", delta, "r1", "s1"); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	got, _, err := store.Materialize(context.Background(), scope, "index")
	if err != nil || len(got.Entries) != 0 {
		t.Fatalf("failed publish became visible: %+v, %v", got, err)
	}
	fault.failManifest = false
	restarted := newTestStore(t, base, DefaultThresholds())
	if _, err := restarted.ApplyDelta(context.Background(), scope, "index", delta, "r1", "s1"); err != nil {
		t.Fatal(err)
	}
	got, _, err = restarted.Materialize(context.Background(), scope, "index")
	if err != nil || len(got.Entries) != 1 || got.Entries[0].ID != "a" {
		t.Fatalf("retry = %+v, %v", got, err)
	}
}

func TestTypedStoreCompactsAndInvalidatesCache(t *testing.T) {
	store := newTestStore(t, workspace.NewMemWorkspace(), Thresholds{MaxSegments: 1, MaxDeltaBytes: 1 << 20})
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(context.Background(), scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Materialize(context.Background(), scope, "index"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyDelta(context.Background(), scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "a", Value: "one"}}}, "r1", "s1"); err != nil {
		t.Fatal(err)
	}
	compactingDelta := EntryDelta[testEntry]{Upserts: []testEntry{{ID: "b", Value: "two"}}}
	result, err := store.ApplyDelta(context.Background(), scope, "index", compactingDelta, "r2", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || len(result.Manifest.Segments) != 0 {
		t.Fatalf("compaction result = %+v", result)
	}
	replayed, err := store.ApplyDelta(context.Background(), scope, "index", compactingDelta, "r2", "s2")
	if err != nil || !replayed.Replayed || replayed.Manifest.Identity != result.Manifest.Identity {
		t.Fatalf("compaction replay = %+v, %v", replayed, err)
	}
	if _, err := store.ApplyDelta(context.Background(), scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "c", Value: "three"}}}, "r3", "s3"); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Materialize(context.Background(), scope, "index")
	if err != nil || len(got.Entries) != 3 {
		t.Fatalf("materialized = %+v, %v", got, err)
	}
}

func TestTypedStoreRejectsCorruptSegmentChain(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := newTestStore(t, ws, DefaultThresholds())
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(context.Background(), scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyDelta(context.Background(), scope, "index",
		EntryDelta[testEntry]{Upserts: []testEntry{{ID: "a", Value: "one"}}}, "r1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	segmentPath := store.segmentPath(scope, "index", result.Manifest.Segments[0].ID)
	if err := ws.Write(context.Background(), segmentPath, []byte(`{"schema_version":2}`)); err != nil {
		t.Fatal(err)
	}
	restarted := newTestStore(t, ws, DefaultThresholds())
	if _, _, err := restarted.Materialize(context.Background(), scope, "index"); err == nil {
		t.Fatal("accepted corrupt segment")
	}
}

func TestTypedStoreConcurrentApplyAndMaterialize(t *testing.T) {
	store := newTestStore(t, workspace.NewMemWorkspace(), Thresholds{MaxSegments: 128, MaxDeltaBytes: 1 << 20})
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(context.Background(), scope, "index", testBase{}, "base"); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 64)
	for index := 0; index < 32; index++ {
		index := index
		wait.Add(2)
		go func() {
			defer wait.Done()
			_, err := store.ApplyDelta(context.Background(), scope, "index",
				EntryDelta[testEntry]{Upserts: []testEntry{{
					ID: fmt.Sprintf("item-%02d", index), Value: "value",
				}}}, fmt.Sprintf("r-%02d", index), fmt.Sprintf("s-%02d", index))
			errs <- err
		}()
		go func() {
			defer wait.Done()
			_, _, err := store.Materialize(context.Background(), scope, "index")
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := store.Materialize(context.Background(), scope, "index")
	if err != nil || len(got.Entries) != 32 {
		t.Fatalf("entries=%d, err=%v", len(got.Entries), err)
	}
}

type meterWorkspace struct {
	workspace.Workspace
	mu    sync.Mutex
	bytes int
}

func (meter *meterWorkspace) Write(ctx context.Context, name string, data []byte) error {
	meter.mu.Lock()
	meter.bytes += len(data)
	meter.mu.Unlock()
	return meter.Workspace.Write(ctx, name, data)
}

func (meter *meterWorkspace) reset() {
	meter.mu.Lock()
	meter.bytes = 0
	meter.mu.Unlock()
}

type manifestFaultWorkspace struct {
	workspace.Workspace
	failManifest bool
}

func (fault *manifestFaultWorkspace) Rename(ctx context.Context, source, destination string) error {
	if fault.failManifest && strings.HasSuffix(destination, "/active.json") {
		return errors.New("injected manifest failure")
	}
	return fault.Workspace.Rename(ctx, source, destination)
}

var _ interface {
	Read(context.Context, string) ([]byte, error)
	Write(context.Context, string, []byte) error
	Append(context.Context, string, []byte) error
	Rename(context.Context, string, string) error
	Delete(context.Context, string) error
	RemoveAll(context.Context, string) error
	List(context.Context, string) ([]fs.DirEntry, error)
	Exists(context.Context, string) (bool, error)
	Stat(context.Context, string) (fs.FileInfo, error)
} = (*meterWorkspace)(nil)
