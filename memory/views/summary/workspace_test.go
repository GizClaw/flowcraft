package summary

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var summaryScope = sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}

func TestWorkspaceStoreAddIdempotentConflictGenerationAndClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	ws := workspace.NewMemWorkspace()
	store, err := NewWorkspaceStore(ws, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
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
	reopened, _ := NewWorkspaceStore(ws)
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

func TestWorkspaceStorePublishesAndReopensActiveManifest(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store, _ := NewWorkspaceStore(ws)
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
	reopened, _ := NewWorkspaceStore(ws)
	got, ok, err := reopened.LoadActive(ctx, summaryScope, "conversation")
	if err != nil || !ok || !reflect.DeepEqual(got.RecordIDs, manifest.RecordIDs) ||
		got.GenerationID != manifest.GenerationID || got.FrontierDigest != manifest.FrontierDigest {
		t.Fatalf("manifest=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestSearcherReturnsOnlyActiveManifestRecords(t *testing.T) {
	ctx := context.Background()
	store, _ := NewWorkspaceStore(workspace.NewMemWorkspace())
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

func TestWorkspaceStoreAddDoesNotListRecords(t *testing.T) {
	ctx := context.Background()
	ws := &listCountingWorkspace{Workspace: workspace.NewMemWorkspace()}
	store, _ := NewWorkspaceStore(ws)
	request := summaryRequest("")
	request.ID = ""
	if _, err := store.Add(ctx, request); err != nil {
		t.Fatal(err)
	}
	if ws.lists != 0 {
		t.Fatalf("Add listed records %d times", ws.lists)
	}
}

func TestWorkspaceStoreActiveCatalogDeltaIsBoundedAndOmitsRecordIDsFromHead(t *testing.T) {
	ctx := context.Background()
	ws := &activeCatalogWorkspace{Workspace: workspace.NewMemWorkspace()}
	store, err := NewWorkspaceStore(ws, WithActiveCompactionThreshold(128))
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
	ws.resetCounts()
	last := summaryRequest("summary-last")
	last.InputIDs = []string{"summary-last"}
	last.CoverageRange = CoverageRange{StartSeq: 1, EndSeq: 66}
	if _, err := store.Add(ctx, last); err != nil {
		t.Fatal(err)
	}
	ws.resetCounts()
	ids = append(ids, last.ID)
	if err := store.PublishActive(ctx, Manifest{
		Scope: summaryScope, ConversationID: "conversation", GenerationID: "generation-1",
		RecordIDs: ids, CoverageRange: last.CoverageRange, FrontierDigest: "frontier-last",
	}); err != nil {
		t.Fatal(err)
	}
	publishReads, publishWritten := ws.reads, ws.written
	if publishReads > 2 || publishWritten > 4096 {
		t.Fatalf("single delta reads=%d written=%d", publishReads, publishWritten)
	}
	head, err := ws.Read(ctx, store.manifestPath(summaryScope, "conversation"))
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

func TestWorkspaceStoreActiveCatalogCrashReplayRemovalAndGenerationSwitch(t *testing.T) {
	ctx := context.Background()
	ws := &activeCatalogWorkspace{Workspace: workspace.NewMemWorkspace()}
	store, _ := NewWorkspaceStore(ws, WithActiveCompactionThreshold(2))
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
	ws.failSegmentRename = true
	if err := store.PublishActive(ctx, next); err == nil {
		t.Fatal("crash before active-segment publish succeeded")
	}
	ws.failSegmentRename = false
	active, err := store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(active), []string{"old", "keep"}) {
		t.Fatalf("post-segment-crash active=%v err=%v", recordIDs(active), err)
	}
	ws.failActiveRename = true
	if err := store.PublishActive(ctx, next); err == nil {
		t.Fatal("crash before active-head publish succeeded")
	}
	ws.failActiveRename = false
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
	reopened, _ := NewWorkspaceStore(ws, WithActiveCompactionThreshold(2))
	active, err = reopened.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(active), []string{"keep", "new"}) {
		t.Fatalf("switched active=%v err=%v", recordIDs(active), err)
	}
	all, err := reopened.List(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || len(all) != 3 {
		t.Fatalf("repair catalog=%v err=%v", recordIDs(all), err)
	}
}

func TestWorkspaceStoreActiveCatalogRejectsBrokenPreviousChain(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store, _ := NewWorkspaceStore(ws, WithActiveCompactionThreshold(128))
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
	headData, err := ws.Read(ctx, store.manifestPath(summaryScope, "conversation"))
	if err != nil {
		t.Fatal(err)
	}
	var head activeCatalogHead
	if err := decodeStrict(headData, &head); err != nil {
		t.Fatal(err)
	}
	segmentPath := store.activeSegmentPath(summaryScope, "conversation", head.HeadSegmentID)
	segmentData, err := ws.Read(ctx, segmentPath)
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
	ws.MustWrite(segmentPath, segmentData)
	ws.MustWrite(store.manifestPath(summaryScope, "conversation"), headData)
	reopened, _ := NewWorkspaceStore(ws)
	if _, err := reopened.ListActive(ctx, summaryScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("broken previous chain accepted")
	}
}

func TestWorkspaceStoreRejectsCorruptionAndInvalidRecord(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store, _ := NewWorkspaceStore(ws)
	ws.MustWrite(store.recordPath(summaryScope, "conversation", "bad"), []byte(`{"schema_version":99}`))
	if _, err := store.List(ctx, summaryScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("corrupt record accepted")
	}
	invalid := summaryRequest("invalid")
	invalid.Level = Level(9)
	if _, err := store.Add(ctx, invalid); err == nil {
		t.Fatal("invalid level accepted")
	}
}

type listCountingWorkspace struct {
	workspace.Workspace
	lists int
}

type activeCatalogWorkspace struct {
	workspace.Workspace
	reads             int
	written           int
	failActiveRename  bool
	failSegmentRename bool
}

func (value *activeCatalogWorkspace) Read(ctx context.Context, name string) ([]byte, error) {
	value.reads++
	return value.Workspace.Read(ctx, name)
}

func (value *activeCatalogWorkspace) Write(ctx context.Context, name string, data []byte) error {
	value.written += len(data)
	return value.Workspace.Write(ctx, name, data)
}

func (value *activeCatalogWorkspace) Rename(ctx context.Context, source, destination string) error {
	if value.failSegmentRename && strings.Contains(destination, "/segments/") {
		return errors.New("injected active-segment failure")
	}
	if value.failActiveRename && strings.HasSuffix(destination, "/active.json") {
		return errors.New("injected active-head failure")
	}
	return value.Workspace.Rename(ctx, source, destination)
}

func (value *activeCatalogWorkspace) resetCounts() {
	value.reads = 0
	value.written = 0
}

func recordIDs(records []Record) []string {
	result := make([]string, len(records))
	for index := range records {
		result[index] = records[index].ID
	}
	return result
}

func (value *listCountingWorkspace) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	value.lists++
	return value.Workspace.List(ctx, dir)
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
