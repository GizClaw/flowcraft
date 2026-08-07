package summary

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var summaryScope = sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}

func TestSummaryStoreAddIdempotentConflictGenerationAndClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	ws := workspace.NewMemWorkspace()
	store := newSummaryStore(t, ws, WithClock(func() time.Time { return now }))
	request := summaryRequest("summary-1")
	first, err := store.Add(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.Add(ctx, request)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	conflict := request
	conflict.Text = "different"
	conflict.Content = textContent("different")
	if _, err := store.Add(ctx, conflict); !errdefs.IsConflict(err) {
		t.Fatalf("conflict err=%v", err)
	}
	active, err := store.List(ctx, summaryScope, "conversation", ListOptions{GenerationID: "generation-1"})
	if err != nil || len(active) != 1 {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	first.Topics[0] = "mutated"
	first.InputIDs[0] = "mutated"
	first.SourceRefs[0].ID = "mutated"
	again, ok, err := store.Get(ctx, summaryScope, "conversation", "summary-1")
	if err != nil || !ok || again.Topics[0] != "topic" || again.InputIDs[0] != "input-1" ||
		again.SourceRefs[0].ID != "conversation/message-1" {
		t.Fatalf("stored summary aliased: %#v ok=%v err=%v", again, ok, err)
	}
	reopened := newSummaryStore(t, ws)
	if _, ok, err := reopened.Get(ctx, summaryScope, "conversation", "summary-1"); err != nil || !ok {
		t.Fatalf("reopen ok=%v err=%v", ok, err)
	}
}

func TestStableIDPreservesInputOrder(t *testing.T) {
	first := StableID(summaryScope, "conversation", L1, []string{"a", "b"}, "source", "transform")
	reordered := StableID(summaryScope, "conversation", L1, []string{"b", "a"}, "source", "transform")
	if first == reordered {
		t.Fatalf("ordered inputs did not affect stable id: %q", first)
	}
}

func TestSummaryStorePublishesAndReopensActiveManifest(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newSummaryStore(t, ws)
	old := summaryRequest("old-tail")
	old.GenerationID = "generation-1"
	if _, err := store.Add(ctx, old); err != nil {
		t.Fatal(err)
	}
	current := summaryRequest("current")
	current.GenerationID = "generation-2"
	if _, err := store.Add(ctx, current); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-2",
		RecordIDs: []string{"current"}, CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 1},
		FrontierDigest: "frontier",
	}
	if err := store.PublishActive(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || len(active) != 1 || active[0].ID != "current" {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	reopened := newSummaryStore(t, ws)
	got, ok, err := reopened.LoadActive(ctx, summaryScope, "conversation")
	if err != nil || !ok || !reflect.DeepEqual(got.RecordIDs, manifest.RecordIDs) ||
		got.GenerationID != manifest.GenerationID || got.FrontierDigest != manifest.FrontierDigest {
		t.Fatalf("manifest=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestSearcherReturnsOnlyActiveManifestRecords(t *testing.T) {
	ctx := context.Background()
	store := newSummaryStore(t, workspace.NewMemWorkspace())
	old := summaryRequest("old-tail")
	old.Text, old.Content = "obsolete tail", textContent("obsolete tail")
	old.GenerationID = "generation-1"
	if _, err := store.Add(ctx, old); err != nil {
		t.Fatal(err)
	}
	current := summaryRequest("current")
	current.Text, current.Content = "current memory", textContent("current memory")
	current.GenerationID = "generation-2"
	if _, err := store.Add(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishActive(ctx, Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-2",
		RecordIDs: []string{"current"}, CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 1},
		FrontierDigest: "frontier",
	}); err != nil {
		t.Fatal(err)
	}
	searcher := &Searcher{Store: store}
	got, err := searcher.Search(ctx, component.SearchRequest{
		Scope: summaryScope, Query: "", Metadata: sdkmemory.Metadata{"conversation_id": "conversation"},
	})
	if err != nil || len(got) != 1 || got[0].ID != "current" || got[0].Metadata["generation_id"] != "generation-2" {
		t.Fatalf("active search=%#v err=%v", got, err)
	}
	oldGeneration, err := searcher.Search(ctx, component.SearchRequest{
		Scope: summaryScope, Query: "",
		Metadata: sdkmemory.Metadata{"conversation_id": "conversation", "generation_id": "generation-1"},
	})
	if err != nil || len(oldGeneration) != 0 {
		t.Fatalf("old generation search=%#v err=%v", oldGeneration, err)
	}
}

func TestSummaryStoreAddDoesNotListRecords(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingKV{Store: kvStore}
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSummaryStore(logStore, counting)
	if err != nil {
		t.Fatal(err)
	}
	request := summaryRequest("")
	request.ID = ""
	if _, err := store.Add(ctx, request); err != nil {
		t.Fatal(err)
	}
	if counting.lists != 0 {
		t.Fatalf("Add listed records %d times", counting.lists)
	}
}

func TestSummaryStoreActiveCatalogDeltaIsBoundedAndOmitsRecordIDsFromHead(t *testing.T) {
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
	store, err := NewSummaryStore(logStore, counting, WithActiveCompactionThreshold(128))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 65)
	for index := 0; index < 65; index++ {
		id := "summary-" + time.Unix(int64(index), 0).UTC().Format("150405")
		request := summaryRequest(id)
		request.InputIDs = []string{id}
		request.CoverageRange = CoverageRange{StartSeq: 1, EndSeq: uint64(index + 1)}
		if _, err := store.Add(ctx, request); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := store.PublishActive(ctx, Manifest{
			Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
			RecordIDs: append([]string(nil), ids...), CoverageRange: request.CoverageRange,
			FrontierDigest: "frontier-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	counting.reset()
	last := summaryRequest("summary-last")
	last.InputIDs = []string{"summary-last"}
	last.CoverageRange = CoverageRange{StartSeq: 1, EndSeq: 66}
	if _, err := store.Add(ctx, last); err != nil {
		t.Fatal(err)
	}
	counting.reset()
	ids = append(ids, last.ID)
	if err := store.PublishActive(ctx, Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
		RecordIDs: ids, CoverageRange: last.CoverageRange, FrontierDigest: "frontier-last",
	}); err != nil {
		t.Fatal(err)
	}
	publishReads, publishWritten := counting.reads, counting.written
	if publishReads > 2 || publishWritten > 4096 {
		t.Fatalf("single delta reads=%d written=%d", publishReads, publishWritten)
	}
	headKey, err := store.manifestPath(summaryScope, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	head, err := kvStore.Get(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("single +1 publish: reads=%d written=%d head=%d", publishReads, publishWritten, len(head))
	if strings.Contains(string(head), "record_ids") || len(head) > 2048 {
		t.Fatalf("active head is not tiny: bytes=%d body=%s", len(head), head)
	}
	active, err := store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || len(active) != len(ids) || active[len(active)-1].ID != last.ID {
		t.Fatalf("active=%d err=%v", len(active), err)
	}
}

func TestSummaryStoreActiveCatalogCrashReplayRemovalAndGenerationSwitch(t *testing.T) {
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
	store, err := NewSummaryStore(logStore, counting, WithActiveCompactionThreshold(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "keep", "new"} {
		request := summaryRequest(id)
		request.InputIDs = []string{id}
		if id == "new" {
			request.GenerationID = "generation-2"
		}
		if _, err := store.Add(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	first := Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
		RecordIDs: []string{"old", "keep"}, CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 1},
		FrontierDigest: "frontier-1",
	}
	if err := store.PublishActive(ctx, first); err != nil {
		t.Fatal(err)
	}
	next := Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-2",
		RecordIDs: []string{"keep", "new"}, CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 2},
		FrontierDigest: "frontier-2",
	}
	counting.failSegment = true
	if err := store.PublishActive(ctx, next); err == nil {
		t.Fatal("crash before active-segment publish succeeded")
	}
	counting.failSegment = false
	active, err := store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(active), []string{"old", "keep"}) {
		t.Fatalf("post-segment-crash active=%v err=%v", recordIDs(active), err)
	}
	counting.failActive = true
	if err := store.PublishActive(ctx, next); err == nil {
		t.Fatal("crash before active-head publish succeeded")
	}
	counting.failActive = false
	active, err = store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(active), []string{"old", "keep"}) {
		t.Fatalf("post-crash active=%v err=%v", recordIDs(active), err)
	}
	if err := store.PublishActive(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishActive(ctx, next); err != nil {
		t.Fatalf("replay: %v", err)
	}
	reopenedStore, err := NewSummaryStore(logStore, kvStore, WithActiveCompactionThreshold(2))
	if err != nil {
		t.Fatal(err)
	}
	active, err = reopenedStore.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(active), []string{"keep", "new"}) {
		t.Fatalf("switched active=%v err=%v", recordIDs(active), err)
	}
	all, err := reopenedStore.List(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || len(all) != 3 {
		t.Fatalf("repair catalog=%v err=%v", recordIDs(all), err)
	}
}

func TestSummaryStoreActiveCatalogRejectsBrokenPreviousChain(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSummaryStore(logStore, kvStore, WithActiveCompactionThreshold(128))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		request := summaryRequest(id)
		request.InputIDs = []string{id}
		if _, err := store.Add(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	for index, ids := range [][]string{{"first"}, {"first", "second"}} {
		if err := store.PublishActive(ctx, Manifest{
			Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
			RecordIDs: ids, CoverageRange: CoverageRange{StartSeq: 1, EndSeq: uint64(index + 1)},
			FrontierDigest: "frontier",
		}); err != nil {
			t.Fatal(err)
		}
	}
	headKey, err := store.manifestPath(summaryScope, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	headData, err := kvStore.Get(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	var head activeCatalogHead
	if err := decodeStrict(headData, &head); err != nil {
		t.Fatal(err)
	}
	segmentKey, err := store.activeSegmentPath(summaryScope, "conversation", head.HeadSegmentID)
	if err != nil {
		t.Fatal(err)
	}
	segmentData, err := kvStore.Get(ctx, segmentKey)
	if err != nil {
		t.Fatal(err)
	}
	var segment activeCatalogSegment
	if err := decodeStrict(segmentData, &segment); err != nil {
		t.Fatal(err)
	}
	segment.PreviousSegmentID = "forged"
	segment.PreviousSegmentDigest = "forged"
	segment.Digest = ""
	segment.Digest = digestCatalog(segment)
	head.HeadSegmentDigest = segment.Digest
	segmentData, _ = json.Marshal(segment)
	headData, _ = json.Marshal(head)
	if err := kvStore.Put(ctx, segmentKey, segmentData); err != nil {
		t.Fatal(err)
	}
	if err := kvStore.Put(ctx, headKey, headData); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSummaryStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ListActive(ctx, summaryScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("broken previous chain accepted")
	}
}

func TestSummaryStoreRejectsCorruptionAndInvalidRecord(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store := newSummaryStore(t, ws)
	badKey, keyErr := store.recordPath(summaryScope, "conversation", "bad")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if err := kvStore.Put(ctx, badKey, []byte(`{"schema_version":99}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, summaryScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("corrupt record accepted")
	}
	invalid := summaryRequest("invalid")
	invalid.Level = Level(9)
	if _, err := store.Add(ctx, invalid); err == nil {
		t.Fatal("invalid level accepted")
	}
}

func TestLevelAndCoverageValidation(t *testing.T) {
	for _, level := range []Level{L0, L1, L2, L3} {
		if err := level.Validate(); err != nil {
			t.Fatalf("%s: %v", level, err)
		}
	}
	if err := (CoverageRange{StartSeq: 5, EndSeq: 4}).Validate(); err == nil {
		t.Fatal("reversed sequence range accepted")
	}
	if err := (CoverageRange{StartTime: time.Now(), EndTime: time.Now().Add(-time.Second)}).Validate(); err == nil {
		t.Fatal("reversed time range accepted")
	}
}

func summaryRequest(id string) AddRequest {
	return AddRequest{
		ID: id, Scope: summaryScope, ConversationID: "conversation", Level: L1,
		Text: "stable summary", Content: textContent("stable summary"),
		Topics: []string{"topic"}, InputIDs: []string{"input-1"},
		SourceRefs: []sdkmemory.SourceRef{{
			Kind: sdkmemory.SourceMessage, ID: "conversation/message-1", Revision: "1",
		}},
		CoverageRange: CoverageRange{StartSeq: 1, EndSeq: 1},
		SourceDigest:  "source-digest", TransformSignature: "summary-v1", GenerationID: "generation-1",
	}
}

func textContent(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
}

func newSummaryStore(t *testing.T, ws workspace.Workspace, options ...Option) *SummaryStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSummaryStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type countingKV struct {
	storage.Store
	mu          sync.Mutex
	reads       int
	written     int
	lists       int
	failSegment bool
	failActive  bool
}

func (kv *countingKV) Get(ctx context.Context, key string) ([]byte, error) {
	kv.mu.Lock()
	kv.reads++
	kv.mu.Unlock()
	return kv.Store.Get(ctx, key)
}

func (kv *countingKV) Put(ctx context.Context, key string, data []byte) error {
	kv.mu.Lock()
	kv.written += len(data)
	if kv.failSegment && strings.Contains(key, "/segments/") {
		kv.mu.Unlock()
		return errors.New("injected active-segment failure")
	}
	if kv.failActive && strings.HasSuffix(key, "/active.json") {
		kv.mu.Unlock()
		return errors.New("injected active-head failure")
	}
	kv.mu.Unlock()
	return kv.Store.Put(ctx, key, data)
}

func (kv *countingKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	kv.mu.Lock()
	kv.written += len(data)
	fail := kv.failSegment && strings.Contains(key, "/segments/")
	kv.mu.Unlock()
	if fail {
		return false, errors.New("injected active-segment failure")
	}
	return kv.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}

func (kv *countingKV) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	kv.mu.Lock()
	kv.lists++
	kv.mu.Unlock()
	return kv.Store.List(ctx, prefix)
}

func (kv *countingKV) reset() {
	kv.mu.Lock()
	kv.reads = 0
	kv.written = 0
	kv.lists = 0
	kv.mu.Unlock()
}

func recordIDs(records []Record) []string {
	result := make([]string, len(records))
	for index := range records {
		result[index] = records[index].ID
	}
	return result
}
