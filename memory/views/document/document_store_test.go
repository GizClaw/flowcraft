package document

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	chunkScope  = sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	chunkSource = sdkmemory.SourceRef{Kind: sdkmemory.SourceDocument, ID: "document", Revision: "1"}
)

func TestDocumentViewStoreBuildSafeReplaceAndReopen(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
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
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
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
	ws := workspace.NewMemWorkspace()
	store := newChunkStore(t, ws)
	request := replaceRequest(1, "zero", "one", "two")
	got, err := store.ReplaceDocument(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Chunks[0].Content.Parts[0] = sdkmessage.TextPart{Text: "mutated"}
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

	otherScope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "other"}
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

	malicious := sdkmemory.Scope{RuntimeID: "../runtime", UserID: "../../user"}
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
		kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
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
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
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

	store := newChunkStore(t, workspace.NewMemWorkspace())
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
			Provenance: []sdkmemory.SourceRef{chunkSource},
			Metadata:   sdkmemory.Metadata{"key": "value"},
		}
	}
	return ReplaceRequest{
		Scope: chunkScope, DatasetID: "dataset", DocumentID: "document",
		DocumentVersion: version, Chunks: chunks,
	}
}

func chunkText(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
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
