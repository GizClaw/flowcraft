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

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	documentScopeA = sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice", AgentID: "agent-a"}
	documentScopeB = sdkmemory.Scope{RuntimeID: "runtime", UserID: "bob", AgentID: "agent-b"}
	testSource     = sdkmemory.SourceRef{Kind: sdkmemory.SourceExternal, ID: "source-1", Locator: "file:///source"}
)

func TestDocumentStorePutReplaceAndRetry(t *testing.T) {
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

func TestDocumentStoreHardPartitionAndDatasetIsolation(t *testing.T) {
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

func TestDocumentStoreAgentPartitionIsolation(t *testing.T) {
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

func TestDocumentStorePersistsAndPaginatesStably(t *testing.T) {
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

func TestDocumentStoreSiblingWritesAreIsolated(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingKV{Store: kvStore}
	store, err := NewDocumentStore(logStore, counting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-a", "a1", "a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-b", "b1", "b1")); err != nil {
		t.Fatal(err)
	}
	keyA, keyErr := store.currentKey(documentScopeA, "dataset", "doc-a")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	keyB, keyErr := store.currentKey(documentScopeA, "dataset", "doc-b")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc-a", "a2", "a2")); err != nil {
		t.Fatal(err)
	}
	if counting.putCount(keyA) != 2 || counting.putCount(keyB) != 1 {
		t.Fatalf("publish counts: doc-a=%d doc-b=%d", counting.putCount(keyA), counting.putCount(keyB))
	}
}

func TestDocumentStoreIdempotencyKeyIsPerDocument(t *testing.T) {
	ctx := context.Background()
	store := newDocumentStore(t, workspace.NewMemWorkspace())
	for _, id := range []string{"doc-a", "doc-b"} {
		got, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", id, "same-key", id))
		if err != nil || got.Version != 1 || got.DocumentID != id {
			t.Fatalf("Put(%s) = %#v, %v", id, got, err)
		}
	}
}

func TestDocumentStoreConcurrentPut(t *testing.T) {
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

func TestDocumentStoreRecoversEventAfterCurrentPublishCrash(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	currentKey, keyErr := dummyDocumentStore.currentKey(documentScopeA, "dataset", "doc")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	faults := &failKV{Store: kvStore, key: currentKey}
	store, err := NewDocumentStore(logStore, faults)
	if err != nil {
		t.Fatal(err)
	}
	request := documentRequest(documentScopeA, "dataset", "doc", "put-1", "durable")
	if _, err := store.Put(ctx, request); err == nil {
		t.Fatal("current pointer publish failure was not surfaced")
	}

	reopened, err := NewDocumentStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 || events[0].Version != 1 {
		t.Fatalf("durable event after crash = %#v, %v", events, err)
	}
	repaired, err := reopened.Put(ctx, request)
	if err != nil || repaired.Version != 1 {
		t.Fatalf("retry repair = %#v, %v", repaired, err)
	}
	current, ok, err := reopened.Get(ctx, documentScopeA, "dataset", "doc")
	if err != nil || !ok || current.Version != 1 {
		t.Fatalf("current after repair: %#v ok=%v err=%v", current, ok, err)
	}
	events, err = reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 {
		t.Fatalf("retry duplicated event = %#v, %v", events, err)
	}
}

func TestDocumentStoreEventsUseScopeOutboxCursorAcrossDocuments(t *testing.T) {
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

func TestDocumentStoreRetryRepairsIncompletePublication(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	byKey, keyErr := dummyDocumentStore.byKeyKey(documentScopeA, "dataset", "doc", "put-1")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	faults := &failKV{Store: kvStore, key: byKey}
	store, err := NewDocumentStore(logStore, faults)
	if err != nil {
		t.Fatal(err)
	}
	request := documentRequest(documentScopeA, "dataset", "doc", "put-1", "durable")
	if _, err := store.Put(ctx, request); err == nil {
		t.Fatal("by-key publish failure was not surfaced")
	}
	reopened, err := NewDocumentStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := reopened.Put(ctx, request)
	if err != nil || repaired.Version != 1 {
		t.Fatalf("retry repair=%#v err=%v", repaired, err)
	}
	events, err := reopened.ListEvents(ctx, documentScopeA, ListEventOptions{})
	if err != nil || len(events) != 1 || events[0].OutboxSeq != 1 {
		t.Fatalf("repaired events=%#v err=%v", events, err)
	}
}

func TestDocumentStoreOwnsInputsAndResults(t *testing.T) {
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

func TestDocumentStoreDeleteAndDeleteDataset(t *testing.T) {
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

func TestDocumentStoreEncodesMaliciousIDs(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store := newDocumentStore(t, ws)
	scope := sdkmemory.Scope{RuntimeID: "../runtime", UserID: "/../../user"}
	dataset := "../../escape/dataset"
	documentID := "/../../document"
	got, err := store.Put(ctx, documentRequest(scope, dataset, documentID, "../key", "safe"))
	if err != nil || got.DocumentID != documentID {
		t.Fatalf("Put = %#v, %v", got, err)
	}
	currentKey, keyErr := store.currentKey(scope, dataset, documentID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if strings.Contains(currentKey, "..") || strings.Contains(currentKey, dataset) ||
		strings.HasPrefix(currentKey, "/") {
		t.Fatalf("unsafe current key %q", currentKey)
	}
	if _, err := kvStore.Get(ctx, currentKey); err != nil {
		t.Fatalf("encoded current missing: %v", err)
	}
}

func TestDocumentStoreRejectsCorruptionAndUnknownSchema(t *testing.T) {
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
			logStore, err := storage.NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			kvStore, err := storage.NewWorkspaceKV(ws)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewDocumentStore(logStore, kvStore)
			if err != nil {
				t.Fatal(err)
			}
			key, keyErr := store.currentKey(documentScopeA, "dataset", "doc")
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			if err := kvStore.Put(ctx, key, test.data); err != nil {
				t.Fatal(err)
			}
			if _, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err == nil {
				t.Fatal("List error = nil")
			}
		})
	}

	ws := workspace.NewMemWorkspace()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store := newDocumentStore(t, ws)
	if _, err := store.Put(ctx, documentRequest(documentScopeA, "dataset", "doc", "key", "ok")); err != nil {
		t.Fatal(err)
	}
	key, keyErr := store.currentKey(documentScopeA, "dataset", "doc")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	data, _ := kvStore.Get(ctx, key)
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["event"].(map[string]any)["document"].(map[string]any)["version"] = float64(0)
	corrupt, _ := json.Marshal(state)
	if err := kvStore.Put(ctx, key, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, documentScopeA, "dataset", ListOptions{}); err == nil {
		t.Fatal("invalid document was accepted")
	}
}

func TestDocumentStoreValidationAndNilDependencies(t *testing.T) {
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDocumentStore(nil, kvStore); err == nil {
		t.Fatal("nil log accepted")
	}
	if _, err := NewDocumentStore(logStore, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	store, err := NewDocumentStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
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

func newDocumentStore(t *testing.T, ws workspace.Workspace, options ...Option) *DocumentStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDocumentStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

var dummyDocumentStore = mustDocumentStore()

func mustDocumentStore() *DocumentStore {
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		panic(err)
	}
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		panic(err)
	}
	store, err := NewDocumentStore(logStore, kvStore)
	if err != nil {
		panic(err)
	}
	return store
}

type countingKV struct {
	storage.Store
	mu   sync.Mutex
	puts map[string]int
}

func (kv *countingKV) Put(ctx context.Context, key string, data []byte) error {
	kv.mu.Lock()
	if kv.puts == nil {
		kv.puts = make(map[string]int)
	}
	kv.puts[key]++
	kv.mu.Unlock()
	return kv.Store.Put(ctx, key, data)
}

func (kv *countingKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	return kv.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}

func (kv *countingKV) putCount(key string) int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.puts[key]
}

type failKV struct {
	storage.Store
	key    string
	failed bool
}

func (kv *failKV) Put(ctx context.Context, key string, data []byte) error {
	if kv.key == key && !kv.failed {
		kv.failed = true
		return errors.New("injected publish failure")
	}
	return kv.Store.Put(ctx, key, data)
}

func (kv *failKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	if kv.key == key && !kv.failed {
		kv.failed = true
		return false, errors.New("injected publish failure")
	}
	return kv.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}
