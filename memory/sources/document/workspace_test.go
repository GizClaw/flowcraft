package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	documentScopeA = sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice", AgentID: "agent-a"}
	documentScopeB = sdkmemory.Scope{RuntimeID: "runtime", UserID: "bob", AgentID: "agent-b"}
	testSource     = sdkmemory.SourceRef{Kind: sdkmemory.SourceExternal, ID: "source-1", Locator: "file:///source"}
)

func TestWorkspaceStorePutReplaceAndRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := newDocumentStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return now }))
	first, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc", "put-1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.DocumentID != "doc" || !first.CreatedAt.Equal(now) || !first.UpdatedAt.Equal(now) {
		t.Fatalf("first authority fields = %#v", first)
	}

	now = now.Add(time.Minute)
	retryRequest := documentRequest(documentScopeA, "dataset", "doc", "put-1", "retry changed")
	retry, err := store.Put(ctx, retryRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("retry = %#v, want %#v", retry, first)
	}

	second, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc", "put-2", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || !second.CreatedAt.Equal(first.CreatedAt) || !second.UpdatedAt.Equal(now) ||
		second.Content.Text() != "second" {
		t.Fatalf("replacement = %#v", second)
	}
	listed, err := store.List(ctx, documentScopeA, "dataset", ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].Version != 2 {
		t.Fatalf("List = %#v, %v", listed, err)
	}
	events, err := store.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 2 {
		t.Fatalf("ListEvents = %#v, %v", events, err)
	}
	if events[0].Operation != OperationPut || events[0].Version != 1 ||
		events[0].Document == nil || events[0].Document.Content.Text() != "first" ||
		events[1].Operation != OperationPut || events[1].Version != 2 ||
		events[1].Document == nil || events[1].Document.Content.Text() != "second" ||
		events[0].ID == events[1].ID {
		t.Fatalf("immutable events = %#v", events)
	}
}

func TestEventIDRemainsByteCompatibleWithLegacyScopeEncoding(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	legacyInput := strings.Join([]string{
		scope.RuntimeID, scope.UserID, scope.AgentID, "dataset", "document",
		strconv.FormatUint(42, 10), string(OperationPut),
	}, "\x00")
	sum := sha256.Sum256([]byte(legacyInput))
	want := "document-event-" + hex.EncodeToString(sum[:])

	if got := eventID(scope, "dataset", "document", 42, OperationPut); got != want {
		t.Fatalf("eventID() = %q, want %q", got, want)
	}
}

func TestWorkspaceStoreHardPartitionAndDatasetIsolation(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	requests := []PutRequest{
		documentRequest(documentScopeA, "same", "doc", "a", "alice"),
		documentRequest(documentScopeB, "same", "doc", "b", "bob"),
		documentRequest(documentScopeA, "other", "doc", "c", "other"),
	}
	for _, request := range requests {
		if _, err := store.Put(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	alice, _, _ := store.Get(ctx, documentScopeA, "same", "doc")
	bob, _, _ := store.Get(ctx, documentScopeB, "same", "doc")
	if alice.Content.Text() != "alice" || bob.Content.Text() != "bob" {
		t.Fatalf("partition leak: alice=%q bob=%q", alice.Content.Text(), bob.Content.Text())
	}
	datasets, err := store.ListDatasets(ctx, documentScopeA)
	if err != nil || !reflect.DeepEqual(datasets, []string{"other", "same"}) {
		t.Fatalf("datasets = %v, %v", datasets, err)
	}
}

func TestWorkspaceStoreAgentPartitionIsolation(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	scopeA := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user", AgentID: "agent-a"}
	scopeB := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user", AgentID: "agent-b"}
	global := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user"}
	for _, item := range []struct {
		scope sdkmemory.Scope
		text  string
	}{
		{scopeA, "a"}, {scopeB, "b"}, {global, "global"},
	} {
		if _, err := store.Put(ctx, documentRequest(item.scope, "dataset", "doc", item.text, item.text)); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		scope sdkmemory.Scope
		text  string
	}{
		{scopeA, "a"}, {scopeB, "b"}, {global, "global"},
	} {
		got, ok, err := store.Get(ctx, item.scope, "dataset", "doc")
		if err != nil || !ok || got.Content.Text() != item.text {
			t.Fatalf("Get(%v) = %#v, %v, %v", item.scope, got, ok, err)
		}
		events, err := store.ListEvents(ctx, item.scope, ListEventOptions{})
		if err != nil || len(events) != 1 || events[0].Scope != item.scope {
			t.Fatalf("ListEvents(%v) = %#v, %v", item.scope, events, err)
		}
	}
}

func TestWorkspaceStorePersistsAndPaginatesStably(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newDocumentStore(t, ws)
	for _, id := range []string{"z", "a", "m", "b"} {
		if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", id, id, id)); err != nil {
			t.Fatal(err)
		}
	}
	reopened := newDocumentStore(t, ws)
	page, err := reopened.List(ctx, documentScopeA, "dataset", ListOptions{AfterID: "a", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := documentIDs(page); !reflect.DeepEqual(got, []string{"b", "m"}) {
		t.Fatalf("page IDs = %v", got)
	}
	rest, err := reopened.List(ctx, documentScopeA, "dataset", ListOptions{AfterID: "m", Limit: -1})
	if err != nil || !reflect.DeepEqual(documentIDs(rest), []string{"z"}) {
		t.Fatalf("rest = %v, %v", documentIDs(rest), err)
	}
}

func TestWorkspaceStoreDocumentFilesDoNotRewriteSiblings(t *testing.T) {
	ctx := context.Background()
	counting := newCountingWorkspace(workspace.NewMemWorkspace())
	store := newDocumentStore(t, counting)
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-a", "a1", "a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-b", "b1", "b1")); err != nil {
		t.Fatal(err)
	}
	pathA := store.documentPath(documentScopeA, "dataset", "doc-a")
	pathB := store.documentPath(documentScopeA, "dataset", "doc-b")
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-a", "a2", "a2")); err != nil {
		t.Fatal(err)
	}
	if counting.renameCount(pathA) != 2 || counting.renameCount(pathB) != 1 {
		t.Fatalf("publish counts: doc-a=%d doc-b=%d", counting.renameCount(pathA), counting.renameCount(pathB))
	}
}

func TestWorkspaceStoreIdempotencyKeyIsPerDocument(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	for _, id := range []string{"doc-a", "doc-b"} {
		got, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", id, "same-key", id))
		if err != nil || got.Version != 1 || got.DocumentID != id {
			t.Fatalf("Put(%s) = %#v, %v", id, got, err)
		}
	}
}

func TestWorkspaceStoreConcurrentPut(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	const count = 40
	var wait sync.WaitGroup
	errs := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.Put(ctx, documentRequest(
				documentScopeA, "dataset", "doc", fmt.Sprint(index), fmt.Sprint(index),
			))
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
	got, ok, err := store.Get(ctx, documentScopeA, "dataset", "doc")
	if err != nil || !ok || got.Version != count {
		t.Fatalf("Get = version %d, ok %v, err %v", got.Version, ok, err)
	}
}

func TestWorkspaceStoreRecoversEventAfterCurrentPublishCrash(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	faults := &failRenameWorkspace{Workspace: base}
	store := newDocumentStore(t, faults)
	faults.destination = store.currentPath(documentScopeA, "dataset", "doc")
	request := documentRequest(documentScopeA, "dataset", "doc", "put-1", "durable")
	if _, err := store.Put(ctx, request); err == nil {
		t.Fatal("current pointer publish failure was not surfaced")
	}

	reopened := newDocumentStore(t, base)
	events, err := reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 || events[0].Version != 1 {
		t.Fatalf("durable event after crash = %#v, %v", events, err)
	}
	current, ok, err := reopened.Get(ctx, documentScopeA, "dataset", "doc")
	if err != nil || !ok || current.Version != 1 {
		t.Fatalf("event scan did not recover current: %#v ok=%v err=%v", current, ok, err)
	}
	repaired, err := reopened.Put(ctx, request)
	if err != nil || repaired.Version != 1 {
		t.Fatalf("retry repair = %#v, %v", repaired, err)
	}
	events, err = reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 {
		t.Fatalf("retry duplicated event = %#v, %v", events, err)
	}
}

func TestWorkspaceStoreEventsUseScopeOutboxCursorAcrossDocuments(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	for _, item := range []struct {
		document string
		key      string
	}{
		{"z-document", "first"},
		{"a-document", "second"},
		{"z-document", "third"},
	} {
		if _, err := store.Put(ctx, documentRequest(
			documentScopeA, "dataset", item.document, item.key, item.key,
		)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ListEvents(ctx, documentScopeA, ListEventOptions{Limit: 2})
	if err != nil || len(first) != 2 || first[0].OutboxSeq != 1 || first[1].OutboxSeq != 2 {
		t.Fatalf("first page=%#v err=%v", first, err)
	}
	second, err := store.ListEvents(ctx, documentScopeA, ListEventOptions{
		AfterOutboxSeq: first[1].OutboxSeq,
		Limit:          2,
	})
	if err != nil || len(second) != 1 || second[0].OutboxSeq != 3 ||
		second[0].DocumentID != "z-document" || second[0].Version != 2 {
		t.Fatalf("second page=%#v err=%v", second, err)
	}
	documentEvents, err := store.ListDocumentEvents(
		ctx, documentScopeA, "dataset", "z-document",
		ListDocumentEventOptions{AfterVersion: 1, Limit: 1},
	)
	if err != nil || len(documentEvents) != 1 || documentEvents[0].Version != 2 ||
		documentEvents[0].OutboxSeq != 3 {
		t.Fatalf("document page=%#v err=%v", documentEvents, err)
	}
}

func TestWorkspaceStoreRepairsEventPublishedBeforeOutboxHead(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	faults := &failRenameWorkspace{Workspace: base}
	store := newDocumentStore(t, faults)
	faults.destination = store.outboxHeadPath(documentScopeA)
	request := documentRequest(documentScopeA, "dataset", "doc", "put-1", "durable")
	if _, err := store.Put(ctx, request); err == nil {
		t.Fatal("outbox head publish failure was not surfaced")
	}

	reopened := newDocumentStore(t, base)
	repaired, err := reopened.Put(ctx, request)
	if err != nil || repaired.Version != 1 {
		t.Fatalf("retry repair=%#v err=%v", repaired, err)
	}
	events, err := reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 || events[0].OutboxSeq != 1 {
		t.Fatalf("repaired events=%#v err=%v", events, err)
	}
}

func TestWorkspaceStoreRepairsFailureAfterOutboxHeadPublication(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	faults := &failDeleteWorkspace{Workspace: base}
	store := newDocumentStore(t, faults)
	faults.target = store.outboxPendingPath(documentScopeA)
	if _, err := store.Put(ctx, documentRequest(
		documentScopeA, "dataset", "doc", "put-1", "durable",
	)); err == nil {
		t.Fatal("post-head cleanup failure was not surfaced")
	}

	reopened := newDocumentStore(t, base)
	events, err := reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 || events[0].OutboxSeq != 1 {
		t.Fatalf("repair after head=%#v err=%v", events, err)
	}
}

func TestWorkspaceStoreOwnsInputsAndResults(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	raw := json.RawMessage(`{"value":"original"}`)
	content := sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.DataPart{Value: raw}}}
	provenance := []sdkmemory.SourceRef{testSource}
	metadata := sdkmemory.Metadata{"key": "original"}
	request := PutRequest{
		Scope: documentScopeA, DatasetID: "dataset", DocumentID: "doc", IdempotencyKey: "key",
		Content: content, Provenance: provenance, Metadata: metadata,
	}
	put, err := store.Put(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	raw[10] = 'X'
	provenance[0].ID = "input mutation"
	metadata["key"] = "input mutation"
	put.Metadata["key"] = "result mutation"
	put.Provenance[0].ID = "result mutation"
	part := put.Content.Parts[0].(sdkmessage.DataPart)
	part.Value[10] = 'Y'

	got, _, err := store.Get(ctx, documentScopeA, "dataset", "doc")
	if err != nil {
		t.Fatal(err)
	}
	gotPart := got.Content.Parts[0].(sdkmessage.DataPart)
	if string(gotPart.Value) != `{"value":"original"}` || got.Provenance[0].ID != "source-1" ||
		got.Metadata["key"] != "original" {
		t.Fatalf("stored value was aliased: %#v", got)
	}
	gotPart.Value[10] = 'Z'
	got.Metadata["key"] = "get mutation"
	again, _, _ := store.Get(ctx, documentScopeA, "dataset", "doc")
	if string(again.Content.Parts[0].(sdkmessage.DataPart).Value) != `{"value":"original"}` ||
		again.Metadata["key"] != "original" {
		t.Fatal("Get result aliases stored document")
	}
}

func TestWorkspaceStoreDeleteAndDeleteDataset(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	for _, item := range []struct{ dataset, id string }{
		{"one", "a"}, {"one", "b"}, {"two", "c"},
	} {
		if _, err := store.Put(ctx, documentRequest(documentScopeA, item.dataset, item.id, item.id, item.id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, documentScopeA, "one", "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, documentScopeA, "one", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, documentScopeA, "one", "a"); ok {
		t.Fatal("deleted document remains")
	}
	events, err := store.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var tombstone Event
	for _, event := range events {
		if event.DatasetID == "one" && event.DocumentID == "a" && event.Operation == OperationTombstone {
			tombstone = event
		}
	}
	if tombstone.ID == "" || tombstone.Version != 2 || tombstone.Document != nil ||
		len(tombstone.Provenance) == 0 {
		t.Fatalf("delete tombstone = %#v", tombstone)
	}
	if err := store.DeleteDataset(ctx, documentScopeA, "one"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDataset(ctx, documentScopeA, "one"); err != nil {
		t.Fatal(err)
	}
	datasets, err := store.ListDatasets(ctx, documentScopeA)
	if err != nil || !reflect.DeepEqual(datasets, []string{"two"}) {
		t.Fatalf("datasets = %v, %v", datasets, err)
	}
	events, err = store.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tombstonedB := false
	for _, event := range events {
		if event.DatasetID == "one" && event.DocumentID == "b" && event.Operation == OperationTombstone {
			tombstonedB = true
		}
	}
	if !tombstonedB {
		t.Fatal("DeleteDataset did not publish a tombstone for document b")
	}
}

func TestWorkspaceStoreEncodesMaliciousIDs(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newDocumentStore(t, ws)
	scope := sdkmemory.Scope{RuntimeID: "../runtime", UserID: "/../../user"}
	dataset := "../../escape/dataset"
	documentID := "/../../document"
	got, err := store.Put(ctx, documentRequest(scope, dataset, documentID, "../key", "safe"))
	if err != nil || got.DocumentID != documentID {
		t.Fatalf("Put = %#v, %v", got, err)
	}
	documentPath := store.documentPath(scope, dataset, documentID)
	if strings.Contains(documentPath, "..") || strings.Contains(documentPath, dataset) || strings.HasPrefix(documentPath, "/") {
		t.Fatalf("unsafe document path %q", documentPath)
	}
	if exists, err := ws.Exists(ctx, documentPath); err != nil || !exists {
		t.Fatalf("encoded document missing: exists=%v err=%v", exists, err)
	}
}

func TestWorkspaceStoreRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"malformed", []byte(`{"schema_version":`)},
		{"unknown schema", []byte(`{"schema_version":99,"runtime_id":"runtime","user_id":"alice","dataset_id":"dataset"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ws := workspace.NewMemWorkspace()
			store := newDocumentStore(t, ws)
			ws.MustWrite(store.documentPath(documentScopeA, "dataset", "doc"), test.data)
			if _, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err == nil {
				t.Fatal("List error = nil")
			}
		})
	}

	ws := workspace.NewMemWorkspace()
	store := newDocumentStore(t, ws)
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc", "key", "ok")); err != nil {
		t.Fatal(err)
	}
	data, _ := ws.Read(ctx, store.eventPath(documentScopeA, "dataset", "doc", "key"))
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["event"].(map[string]any)["document"].(map[string]any)["version"] = float64(0)
	corrupt, _ := json.Marshal(state)
	ws.MustWrite(store.eventPath(documentScopeA, "dataset", "doc", "key"), corrupt)
	if _, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err == nil {
		t.Fatal("invalid document was accepted")
	}
}

func TestWorkspaceStoreIgnoresForeignFilesButRejectsDataLikeNames(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newDocumentStore(t, ws)
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc", "key", "ok")); err != nil {
		t.Fatal(err)
	}
	ws.MustWrite(store.documentsDir(documentScopeA, "dataset")+"/README.txt", []byte("ignored"))
	ws.MustWrite(store.documentsDir(documentScopeA, "dataset")+"/.document.json.tmp", []byte("ignored"))
	if documents, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err != nil || len(documents) != 1 {
		t.Fatalf("foreign files affected scan: documents=%d err=%v", len(documents), err)
	}
	ws.MustWrite(store.documentsDir(documentScopeA, "dataset")+"/k_!!!.json", []byte("{}"))
	if _, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err == nil {
		t.Fatal("data-like invalid filename was ignored")
	}
}

func TestWorkspaceStoreValidationAndNilWorkspace(t *testing.T) {
	if _, err := NewWorkspaceStore(nil); err == nil {
		t.Fatal("nil workspace accepted")
	}
	var typedNil *workspace.MemWorkspace
	if _, err := NewWorkspaceStore(typedNil); err == nil {
		t.Fatal("typed nil workspace accepted")
	}
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	request := documentRequest(documentScopeA, "dataset", "doc", "key", "content")
	request.Provenance = nil
	if _, err := store.Put(context.Background(), request); err == nil {
		t.Fatal("missing provenance accepted")
	}
}

func documentRequest(scope sdkmemory.Scope, dataset, documentID, key, text string) PutRequest {
	return PutRequest{
		Scope: scope, DatasetID: dataset, DocumentID: documentID, IdempotencyKey: key,
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}},
		Provenance: []sdkmemory.SourceRef{testSource},
		Metadata:   sdkmemory.Metadata{"text": text},
	}
}

func documentIDs(documents []Document) []string {
	ids := make([]string, len(documents))
	for index, document := range documents {
		ids[index] = document.DocumentID
	}
	return ids
}

func newDocumentStore(t *testing.T, ws workspace.Workspace, options ...Option) *WorkspaceStore {
	t.Helper()
	store, err := NewWorkspaceStore(ws, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type countingWorkspace struct {
	workspace.Workspace
	mu      sync.Mutex
	renames map[string]int
}

type failRenameWorkspace struct {
	workspace.Workspace
	mu          sync.Mutex
	destination string
	failed      bool
}

type failDeleteWorkspace struct {
	workspace.Workspace
	mu     sync.Mutex
	target string
	failed bool
}

func (ws *failDeleteWorkspace) Delete(ctx context.Context, name string) error {
	ws.mu.Lock()
	if name == ws.target && !ws.failed {
		ws.failed = true
		ws.mu.Unlock()
		return errors.New("injected delete failure")
	}
	ws.mu.Unlock()
	return ws.Workspace.Delete(ctx, name)
}

func (ws *failRenameWorkspace) Rename(ctx context.Context, source, destination string) error {
	ws.mu.Lock()
	if destination == ws.destination && !ws.failed {
		ws.failed = true
		ws.mu.Unlock()
		return errors.New("injected rename failure")
	}
	ws.mu.Unlock()
	return ws.Workspace.Rename(ctx, source, destination)
}

func newCountingWorkspace(ws workspace.Workspace) *countingWorkspace {
	return &countingWorkspace{Workspace: ws, renames: make(map[string]int)}
}

func (ws *countingWorkspace) Rename(ctx context.Context, source, destination string) error {
	if err := ws.Workspace.Rename(ctx, source, destination); err != nil {
		return err
	}
	ws.mu.Lock()
	ws.renames[destination]++
	ws.mu.Unlock()
	return nil
}

func (ws *countingWorkspace) renameCount(path string) int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.renames[path]
}
