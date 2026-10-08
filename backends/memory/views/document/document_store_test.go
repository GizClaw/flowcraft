package document

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

var (
	chunkScope  = corememory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	chunkSource = corememory.SourceRef{Kind: corememory.SourceDocument, ID: "document", Revision: "1"}
)

func TestDocumentViewStoreBuildSafeReplaceAndReopen(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace(t)
	store := newChunkStore(t, ws)
	first := replaceRequest(1, "old-a", "old-b")
	published, err := store.ReplaceDocument(ctx, first)
	if err != nil || len(published) != 2 {
		t.Fatalf("first replace = %#v, %v", published, err)
	}
	second := replaceRequest(2, "new")
	if _, err := store.ReplaceDocument(ctx, second); err != nil {
		t.Fatal(err)
	}
	reopened := newChunkStore(t, ws)
	listed, err := reopened.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Content.Text() != "new" ||
		listed[0].DocumentVersion != 2 {
		t.Fatalf("active chunks = %#v, %v", listed, err)
	}
	if _, ok, err := reopened.Get(ctx, chunkScope, "dataset", "document", first.Chunks[0].ID); err != nil || ok {
		t.Fatalf("old chunk visible: ok=%v err=%v", ok, err)
	}
}

func TestDocumentViewStorePointerFailurePreservesOldBuild(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceDocument(ctx, replaceRequest(1, "old")); err != nil {
		t.Fatal(err)
	}
	activeKey, err := store.activeKey(chunkScope, "dataset", "document")
	if err != nil {
		t.Fatal(err)
	}
	failing := &failPutKV{Store: kvStore, key: activeKey}
	store.kv = failing
	if _, err := store.ReplaceDocument(ctx, replaceRequest(2, "new")); err == nil {
		t.Fatal("pointer failure not surfaced")
	}
	store.kv = kvStore
	listed, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Content.Text() != "old" ||
		listed[0].DocumentVersion != 1 {
		t.Fatalf("old build not preserved: %#v, %v", listed, err)
	}
}

func TestDocumentViewStoreClonePaginationIsolationTraversalAndConcurrency(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace(t)
	store := newChunkStore(t, ws)
	request := replaceRequest(1, "zero", "one", "two")
	got, err := store.ReplaceDocument(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Chunks[0].Content.Parts[0] = coremessage.TextPart{Text: "mutated"}
	request.Chunks[0].Provenance[0].ID = "mutated"
	got[0].Metadata["key"] = "mutated"
	page, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{Limit: 2})
	if err != nil || len(page) != 2 || page[0].Content.Text() != "zero" ||
		page[0].Provenance[0].ID != "document" || page[0].Metadata["key"] != "value" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	rest, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{
		AfterOrdinal: page[1].Ordinal, AfterID: page[1].ID,
	})
	if err != nil || len(rest) != 1 || rest[0].Content.Text() != "two" {
		t.Fatalf("rest = %#v, %v", rest, err)
	}

	otherScope := corememory.Scope{RuntimeID: "runtime", UserID: "other"}
	other := replaceRequest(1, "other")
	other.Scope = otherScope
	other.Chunks[0].Scope = otherScope
	if _, err := store.ReplaceDocument(ctx, other); err != nil {
		t.Fatal(err)
	}
	otherList, _ := store.List(ctx, otherScope, "dataset", "document", ListOptions{})
	if len(otherList) != 1 || otherList[0].Content.Text() != "other" {
		t.Fatalf("partition leak: %#v", otherList)
	}

	malicious := corememory.Scope{RuntimeID: "../runtime", UserID: "../../user"}
	bad := replaceRequest(1, "safe")
	bad.Scope, bad.DatasetID, bad.DocumentID = malicious, "../../dataset", "/../../document"
	bad.Chunks[0].Scope, bad.Chunks[0].DatasetID, bad.Chunks[0].DocumentID =
		malicious, bad.DatasetID, bad.DocumentID
	if _, err := store.ReplaceDocument(ctx, bad); err != nil {
		t.Fatal(err)
	}
	target, err := store.activeKey(malicious, bad.DatasetID, bad.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(target, "..") || strings.HasPrefix(target, "/") {
		t.Fatalf("unsafe key %q", target)
	}

	var wait sync.WaitGroup
	errs := make(chan error, 20)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.ReplaceDocument(ctx, replaceRequest(uint64(index+2), fmt.Sprint(index)))
			errs <- err
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	final, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(final) != 1 || final[0].DocumentVersion != 21 {
		t.Fatalf("concurrent final = %#v, %v", final, err)
	}
}

func TestDocumentViewStoreRejectsCorruptionSchemaAndInvalidReplacement(t *testing.T) {
	ctx := context.Background()
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99}`),
	} {
		kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewDocumentViewStore(kvStore)
		if err != nil {
			t.Fatal(err)
		}
		activeKey, err := store.activeKey(chunkScope, "dataset", "document")
		if err != nil {
			t.Fatal(err)
		}
		if err := kvStore.Put(ctx, activeKey, data); err != nil {
			t.Fatal(err)
		}
		if _, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{}); err == nil {
			t.Fatal("corrupt active pointer accepted")
		}
	}
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	corruptStore, err := NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	request := replaceRequest(1, "text")
	if _, err := corruptStore.ReplaceDocument(ctx, request); err != nil {
		t.Fatal(err)
	}
	active, ok, err := corruptStore.readActive(ctx, chunkScope, "dataset", "document")
	if err != nil || !ok {
		t.Fatalf("read active = %#v, %v, %v", active, ok, err)
	}
	chunkKey, err := corruptStore.chunkKey(chunkScope, "dataset", "document", active.BuildID, request.Chunks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := kvStore.Get(ctx, chunkKey)
	data = []byte(strings.Replace(string(data), `"schema_version":1`, `"schema_version":99`, 1))
	if err := kvStore.Put(ctx, chunkKey, data); err != nil {
		t.Fatal(err)
	}
	if _, err := corruptStore.List(ctx, chunkScope, "dataset", "document", ListOptions{}); err == nil {
		t.Fatal("unknown chunk schema accepted")
	}

	store := newChunkStore(t, newTestWorkspace(t))
	duplicate := replaceRequest(1, "a", "b")
	duplicate.Chunks[1].ID = duplicate.Chunks[0].ID
	if _, err := store.ReplaceDocument(ctx, duplicate); err == nil {
		t.Fatal("duplicate chunk id accepted")
	}
	if _, err := NewDocumentViewStore(nil); err == nil {
		t.Fatal("nil store accepted")
	}
}

func replaceRequest(version uint64, texts ...string) ReplaceRequest {
	chunks := make([]Chunk, len(texts))
	for index, text := range texts {
		chunks[index] = Chunk{
			ID: fmt.Sprintf("v%d-%d-%s", version, index, text), Scope: chunkScope,
			DatasetID: "dataset", DocumentID: "document", DocumentVersion: version,
			Ordinal: uint64(index), Content: chunkText(text),
			Provenance: []corememory.SourceRef{chunkSource},
			Metadata:   corememory.Metadata{"key": "value"},
		}
	}
	return ReplaceRequest{
		Scope: chunkScope, DatasetID: "dataset", DocumentID: "document",
		DocumentVersion: version, Chunks: chunks,
	}
}

func chunkText(text string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}}
}

// TestDocumentViewStoreRejectsParentCycles guards hierarchy walks: a cyclic
// parent chain would make retrieval parent expansion loop forever.
func TestDocumentViewStoreRejectsParentCycles(t *testing.T) {
	ctx := context.Background()
	store := newChunkStore(t, newTestWorkspace(t))
	request := replaceRequest(1, "a", "b")
	request.Chunks[0].ParentID = request.Chunks[1].ID
	request.Chunks[1].ParentID = request.Chunks[0].ID
	if _, err := store.ReplaceDocument(ctx, request); err == nil {
		t.Fatal("parent cycle was accepted")
	}
	request.Chunks[1].ParentID = request.Chunks[0].ID
	request.Chunks[0].ParentID = ""
	if _, err := store.ReplaceDocument(ctx, request); err != nil {
		t.Fatalf("acyclic hierarchy rejected: %v", err)
	}
}

func newChunkStore(t *testing.T, ws workspace.Workspace) *DocumentViewStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type failPutKV struct {
	storage.Store
	key    string
	failed bool
}

func (store *failPutKV) Put(ctx context.Context, key string, data []byte) error {
	if store.key == key && !store.failed {
		store.failed = true
		return errors.New("injected active pointer failure")
	}
	return store.Store.Put(ctx, key, data)
}

func (store *failPutKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	return store.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}

// TestRetireStaleBuildsKeepsOnlyTheActiveBuild is the reclaim fixture: a
// document published twice leaves two immutable builds, reads resolve the
// active one, and the superseded one is unreachable until it is swept.
func TestRetireStaleBuildsKeepsOnlyTheActiveBuild(t *testing.T) {
	ctx := context.Background()
	store := newChunkStore(t, newTestWorkspace(t))
	if _, err := store.ReplaceDocument(ctx, replaceRequest(1, "old-a", "old-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceDocument(ctx, replaceRequest(2, "new")); err != nil {
		t.Fatal(err)
	}
	builds, err := store.ListBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil || len(builds) != 2 {
		t.Fatalf("builds = %#v, %v", builds, err)
	}
	removed, err := store.RetireStaleBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil || removed != 2 {
		t.Fatalf("retire = %d, %v, want the two chunks of the superseded build", removed, err)
	}
	remaining, err := store.ListBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil {
		t.Fatal(err)
	}
	active, found, activeErr := store.readActive(ctx, chunkScope, "dataset", "document")
	if activeErr != nil || !found {
		t.Fatalf("active build = %#v found=%v, %v", active, found, activeErr)
	}
	if len(remaining) != 1 || remaining[0] != active.BuildID {
		t.Fatalf("remaining builds = %#v, want only %q", remaining, active.BuildID)
	}
	listed, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Content.Text() != "new" {
		t.Fatalf("active chunks after retire = %#v, %v", listed, err)
	}
	// The retired chunk is gone, so an address a lane still holds for the
	// superseded build resolves nothing.
	if _, ok, err := store.Get(ctx, chunkScope, "dataset", "document", "v1-0-old-a"); err != nil || ok {
		t.Fatalf("retired chunk still visible: ok=%v err=%v", ok, err)
	}
	// Retiring again finds nothing, so a retried pass is a no-op.
	again, err := store.RetireStaleBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil || again != 0 {
		t.Fatalf("second retire = %d, %v, want 0", again, err)
	}
}

// TestRetireStaleBuildsDropsBuildsNoPointerServes covers the publish that failed
// after writing its chunks: those chunks are unreachable, and a document with no
// active pointer keeps none of its builds.
func TestRetireStaleBuildsDropsBuildsNoPointerServes(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace(t)
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceDocument(ctx, replaceRequest(1, "kept")); err != nil {
		t.Fatal(err)
	}
	activeKey, err := store.activeKey(chunkScope, "dataset", "document")
	if err != nil {
		t.Fatal(err)
	}
	store.kv = &failPutKV{Store: kvStore, key: activeKey}
	if _, err := store.ReplaceDocument(ctx, replaceRequest(2, "orphan-a", "orphan-b")); err == nil {
		t.Fatal("pointer failure not surfaced")
	}
	store.kv = kvStore
	if builds, err := store.ListBuilds(ctx, chunkScope, "dataset", "document"); err != nil || len(builds) != 2 {
		t.Fatalf("builds after failed publish = %#v, %v", builds, err)
	}
	removed, err := store.RetireStaleBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil || removed != 2 {
		t.Fatalf("retire = %d, %v, want the two orphan chunks", removed, err)
	}
	listed, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Content.Text() != "kept" {
		t.Fatalf("active chunks after retire = %#v, %v", listed, err)
	}
	// A document whose pointer never landed has no reader at all, so a sweep
	// keeps none of it.
	if err := kvStore.Delete(ctx, activeKey); err != nil {
		t.Fatal(err)
	}
	removed, err = store.RetireStaleBuilds(ctx, chunkScope, "dataset", "document")
	if err != nil || removed != 1 {
		t.Fatalf("retire without active = %d, %v, want the remaining chunk", removed, err)
	}
	if builds, err := store.ListBuilds(ctx, chunkScope, "dataset", "document"); err != nil || len(builds) != 0 {
		t.Fatalf("builds after retire = %#v, %v", builds, err)
	}
}

// TestRetireScopeBuildsCoversOneScopeOnly sweeps every document of a scope and
// leaves the partitions of other scopes where they are.
func TestRetireScopeBuildsCoversOneScopeOnly(t *testing.T) {
	ctx := context.Background()
	store := newChunkStore(t, newTestWorkspace(t))
	other := corememory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "other"}
	publish := func(scope corememory.Scope, datasetID, documentID string, versions ...uint64) {
		t.Helper()
		for _, version := range versions {
			request := replaceRequest(version, fmt.Sprintf("v%d", version))
			request.Scope = scope
			request.DatasetID = datasetID
			request.DocumentID = documentID
			for index := range request.Chunks {
				request.Chunks[index].Scope = scope
				request.Chunks[index].DatasetID = datasetID
				request.Chunks[index].DocumentID = documentID
			}
			if _, err := store.ReplaceDocument(ctx, request); err != nil {
				t.Fatal(err)
			}
		}
	}
	publish(chunkScope, "dataset", "document", 1, 2, 3)
	publish(chunkScope, "dataset", "other-document", 1, 2)
	publish(chunkScope, "other-dataset", "document", 1, 2)
	publish(other, "dataset", "document", 1, 2)
	removed, err := store.RetireScopeBuilds(ctx, chunkScope)
	if err != nil || removed != 4 {
		t.Fatalf("scope retire = %d, %v, want the 4 superseded chunks", removed, err)
	}
	for _, address := range [][2]string{
		{"dataset", "document"}, {"dataset", "other-document"}, {"other-dataset", "document"},
	} {
		builds, err := store.ListBuilds(ctx, chunkScope, address[0], address[1])
		if err != nil || len(builds) != 1 {
			t.Fatalf("builds of %v = %#v, %v, want only the active one", address, builds, err)
		}
	}
	if builds, err := store.ListBuilds(ctx, other, "dataset", "document"); err != nil || len(builds) != 2 {
		t.Fatalf("builds of another scope = %#v, %v, want both", builds, err)
	}
}
