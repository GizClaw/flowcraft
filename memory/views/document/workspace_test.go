package document

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	chunkScope  = sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	chunkSource = sdkmemory.SourceRef{Kind: sdkmemory.SourceDocument, ID: "document", Revision: "1"}
)

func TestWorkspaceStoreBuildSafeReplaceAndReopen(t *testing.T) {
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

func TestWorkspaceStorePointerFailurePreservesOldBuild(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	failing := &renameFailWorkspace{Workspace: base}
	store := newChunkStore(t, failing)
	if _, err := store.ReplaceDocument(ctx, replaceRequest(1, "old")); err != nil {
		t.Fatal(err)
	}
	failing.destination = store.activePath(chunkScope, "dataset", "document")
	failing.fail = true
	if _, err := store.ReplaceDocument(ctx, replaceRequest(2, "new")); err == nil {
		t.Fatal("pointer failure not surfaced")
	}
	failing.fail = false
	listed, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Content.Text() != "old" ||
		listed[0].DocumentVersion != 1 {
		t.Fatalf("old build not preserved: %#v, %v", listed, err)
	}
}

func TestWorkspaceStoreClonePaginationIsolationTraversalAndConcurrency(t *testing.T) {
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
	target := store.activePath(malicious, bad.DatasetID, bad.DocumentID)
	if strings.Contains(target, "..") || strings.HasPrefix(target, "/") {
		t.Fatalf("unsafe path %q", target)
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

func TestWorkspaceStoreRejectsCorruptionSchemaAndInvalidReplacement(t *testing.T) {
	ctx := context.Background()
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99}`),
	} {
		ws := workspace.NewMemWorkspace()
		store := newChunkStore(t, ws)
		ws.MustWrite(store.activePath(chunkScope, "dataset", "document"), data)
		if _, err := store.List(ctx, chunkScope, "dataset", "document", ListOptions{}); err == nil {
			t.Fatal("corrupt active pointer accepted")
		}
	}
	ws := workspace.NewMemWorkspace()
	corruptStore := newChunkStore(t, ws)
	request := replaceRequest(1, "text")
	if _, err := corruptStore.ReplaceDocument(ctx, request); err != nil {
		t.Fatal(err)
	}
	active, ok, err := corruptStore.readActive(ctx, chunkScope, "dataset", "document")
	if err != nil || !ok {
		t.Fatalf("read active = %#v, %v, %v", active, ok, err)
	}
	chunkPath := corruptStore.chunkPath(chunkScope, "dataset", "document", active.BuildID, request.Chunks[0].ID)
	data, _ := ws.Read(ctx, chunkPath)
	data = []byte(strings.Replace(string(data), `"schema_version":1`, `"schema_version":99`, 1))
	ws.MustWrite(chunkPath, data)
	if _, err := corruptStore.List(ctx, chunkScope, "dataset", "document", ListOptions{}); err == nil {
		t.Fatal("unknown chunk schema accepted")
	}

	store := newChunkStore(t, workspace.NewMemWorkspace())
	duplicate := replaceRequest(1, "a", "b")
	duplicate.Chunks[1].ID = duplicate.Chunks[0].ID
	if _, err := store.ReplaceDocument(ctx, duplicate); err == nil {
		t.Fatal("duplicate chunk id accepted")
	}
	if _, err := NewWorkspaceStore(nil); err == nil {
		t.Fatal("nil workspace accepted")
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

func newChunkStore(t *testing.T, ws workspace.Workspace) *WorkspaceStore {
	t.Helper()
	store, err := NewWorkspaceStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type renameFailWorkspace struct {
	workspace.Workspace
	mu          sync.Mutex
	destination string
	fail        bool
}

func (ws *renameFailWorkspace) Rename(ctx context.Context, source, destination string) error {
	ws.mu.Lock()
	fail := ws.fail && destination == ws.destination
	ws.mu.Unlock()
	if fail {
		return errors.New("injected active pointer failure")
	}
	return ws.Workspace.Rename(ctx, source, destination)
}

func TestWorkspaceStoreEmptyBuildIsActive(t *testing.T) {
	store := newChunkStore(t, workspace.NewMemWorkspace())
	if _, err := store.ReplaceDocument(context.Background(), replaceRequest(1)); err != nil {
		t.Fatal(err)
	}
	got, err := store.List(context.Background(), chunkScope, "dataset", "document", ListOptions{})
	if err != nil || !reflect.DeepEqual(got, []Chunk{}) {
		t.Fatalf("empty active build = %#v, %v", got, err)
	}
}
