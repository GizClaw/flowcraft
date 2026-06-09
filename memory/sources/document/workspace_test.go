package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newTestWorkspace() workspace.Workspace {
	root := workspace.NewMemWorkspace()
	return workspace.Sub(root, "memory/sources/document-test")
}

func TestWorkspaceStore_PutAssignsAuthoritativeFields(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 8, 14, 0, 0, 0, time.UTC)
	callerCreatedAt := now.Add(-time.Hour)
	callerUpdatedAt := now.Add(time.Hour)
	store := NewWorkspaceStore(newTestWorkspace(), WithClock(func() time.Time { return now }))

	got, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID:   "dataset-1",
			ID:          "doc-1",
			Name:        "source.txt",
			SourceURI:   "file:///tmp/source.txt",
			MIMEType:    "text/plain",
			Content:     "raw content",
			Metadata:    map[string]any{"kind": "note"},
			Version:     99,
			ContentHash: "sha256:caller",
			CreatedAt:   callerCreatedAt,
			UpdatedAt:   callerUpdatedAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "doc-1" {
		t.Fatalf("ID = %q, want explicit ID", got.ID)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want store clock %v", got.CreatedAt, now)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want store clock %v", got.UpdatedAt, now)
	}
	if got.ContentHash != wantContentHash("raw content") {
		t.Fatalf("ContentHash = %q, want %q", got.ContentHash, wantContentHash("raw content"))
	}
	if got.Content != "raw content" {
		t.Fatalf("Content = %q, want raw content", got.Content)
	}
	if got.SourceURI != "file:///tmp/source.txt" {
		t.Fatalf("SourceURI = %q, want file:///tmp/source.txt", got.SourceURI)
	}
	if got.MIMEType != "text/plain" {
		t.Fatalf("MIMEType = %q, want text/plain", got.MIMEType)
	}
}

func TestWorkspaceStore_RecreatedStoreReadsPersistedDocuments(t *testing.T) {
	ctx := context.Background()
	root := workspace.NewMemWorkspace()
	ws := workspace.Sub(root, "memory/sources/document-test")
	store := NewWorkspaceStore(ws)

	first, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: "doc-1", Content: "persist me"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-2", ID: "doc-2", Content: "also here"}}); err != nil {
		t.Fatal(err)
	}
	encodedDocumentPath := "datasets/" + pathSegment("dataset-1") + "/documents/" + pathSegment("doc-1") + ".json"
	if exists, err := ws.Exists(ctx, encodedDocumentPath); err != nil || !exists {
		t.Fatalf("document JSON exists = %v err %v, want true nil", exists, err)
	}

	recreated := NewWorkspaceStore(ws)
	got, ok, err := recreated.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get from recreated store ok = false, want true")
	}
	if got.ID != first.ID || got.Content != "persist me" {
		t.Fatalf("Get from recreated store = (%q, %q), want (%q, persist me)", got.ID, got.Content, first.ID)
	}

	listed, err := recreated.List(ctx, "dataset-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, listed, []string{"doc-1"})

	datasets, err := recreated.ListDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, datasets, []string{"dataset-1", "dataset-2"})
}

func TestWorkspaceStore_ReputIncrementsVersionAndPreservesContent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 8, 14, 0, 0, 0, time.UTC)
	store := NewWorkspaceStore(newTestWorkspace(), WithClock(func() time.Time { return now }))

	first, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Content:   "first",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	losslessContent := "second line\nwith bytes \x00 and unicode-ish ascii"
	second, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID:   "dataset-1",
			ID:          "doc-1",
			Content:     losslessContent,
			Version:     500,
			ContentHash: "sha256:caller",
			CreatedAt:   now.Add(-24 * time.Hour),
			UpdatedAt:   now.Add(24 * time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.Version != 1 {
		t.Fatalf("first Version = %d, want 1", first.Version)
	}
	if second.Version != 2 {
		t.Fatalf("second Version = %d, want 2", second.Version)
	}
	if !first.CreatedAt.Equal(time.Date(2026, 6, 8, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("first CreatedAt = %v, want initial clock", first.CreatedAt)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("second CreatedAt = %v, want preserved %v", second.CreatedAt, first.CreatedAt)
	}
	if !second.UpdatedAt.Equal(now) {
		t.Fatalf("second UpdatedAt = %v, want %v", second.UpdatedAt, now)
	}
	if second.Content != losslessContent {
		t.Fatalf("second Content = %q, want %q", second.Content, losslessContent)
	}
	if first.ContentHash != wantContentHash("first") {
		t.Fatalf("first ContentHash = %q, want %q", first.ContentHash, wantContentHash("first"))
	}
	if second.ContentHash != wantContentHash(losslessContent) {
		t.Fatalf("second ContentHash = %q, want %q", second.ContentHash, wantContentHash(losslessContent))
	}
}

func TestWorkspaceStore_PutValidatesRequiredIdentity(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	if _, err := store.Put(ctx, PutRequest{Document: Document{ID: "doc-1"}}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty dataset Put err = %v, want validation", err)
	}
	if _, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", Name: "legacy-name.txt"}}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty id Put err = %v, want validation even when Name is set", err)
	}
}

func TestWorkspaceStore_MetadataClones(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	metadata := map[string]any{"key": "original", "kind": "note"}

	put, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Metadata:  metadata,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata["key"] = "input-mutated"
	put.Metadata["key"] = "put-result-mutated"

	got, ok, err := store.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if got.Metadata["key"] != "original" {
		t.Fatalf("Get metadata = %q, want original", got.Metadata["key"])
	}
	got.Metadata["key"] = "get-mutated"

	listed, err := store.List(ctx, "dataset-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d documents, want 1", len(listed))
	}
	if listed[0].Metadata["key"] != "original" {
		t.Fatalf("List metadata = %q, want original", listed[0].Metadata["key"])
	}
	listed[0].Metadata["key"] = "list-mutated"

	gotAgain, ok, err := store.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("second Get ok = false, want true")
	}
	if gotAgain.Metadata["key"] != "original" {
		t.Fatalf("stored metadata mutated to %q, want original", gotAgain.Metadata["key"])
	}
	if gotAgain.Metadata["kind"] != "note" {
		t.Fatalf("stored metadata kind = %v, want note", gotAgain.Metadata["kind"])
	}
}

func TestCloneDocumentClonesNestedMetadata(t *testing.T) {
	original := Document{
		Metadata: map[string]any{
			"object": map[string]any{"key": "original"},
			"array":  []any{map[string]any{"key": "original"}},
		},
	}

	cloned := cloneDocument(original)
	cloned.Metadata["object"].(map[string]any)["key"] = "mutated"
	cloned.Metadata["array"].([]any)[0].(map[string]any)["key"] = "mutated"

	if original.Metadata["object"].(map[string]any)["key"] != "original" {
		t.Fatalf("cloneDocument shared nested object metadata: %#v", original.Metadata["object"])
	}
	if original.Metadata["array"].([]any)[0].(map[string]any)["key"] != "original" {
		t.Fatalf("cloneDocument shared nested array metadata: %#v", original.Metadata["array"])
	}
}

func TestWorkspaceStore_MetadataJSONRoundTripContract(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	metadata := map[string]any{
		"int":    42,
		"bool":   true,
		"array":  []any{"one", 2, false},
		"object": map[string]any{"nested": 7},
	}
	if _, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Metadata:  metadata,
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}

	want := map[string]any{
		"int":    float64(42),
		"bool":   true,
		"array":  []any{"one", float64(2), false},
		"object": map[string]any{"nested": float64(7)},
	}
	if !reflect.DeepEqual(got.Metadata, want) {
		t.Fatalf("Metadata = %#v, want %#v", got.Metadata, want)
	}
}

func TestWorkspaceStore_IDPrimaryKeyAndNameIsDisplayAlias(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	first, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Name:      "shared-name.txt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-2",
			Name:      "shared-name.txt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := store.List(ctx, "dataset-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, listed, []string{"doc-1", "doc-2"})
	if first.Name != second.Name {
		t.Fatalf("Names = %q and %q, want matching display names", first.Name, second.Name)
	}

	renamed, err := store.Put(ctx, PutRequest{
		Document: Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Name:      "renamed.txt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Version != 2 {
		t.Fatalf("renamed Version = %d, want 2", renamed.Version)
	}
	listed, err = store.List(ctx, "dataset-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, listed, []string{"doc-1", "doc-2"})

	got, ok, err := store.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if got.Name != "renamed.txt" {
		t.Fatalf("Name = %q, want renamed.txt", got.Name)
	}
}

func TestWorkspaceStore_IDsUseSafePathSegments(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		datasetID  string
		documentID string
	}{
		{datasetID: ".", documentID: "."},
		{datasetID: "..", documentID: ".."},
		{datasetID: "dataset/with/slash", documentID: "doc/with/slash"},
		{datasetID: "name%percent", documentID: "doc%percent"},
		{datasetID: "space name", documentID: "space doc"},
		{datasetID: "suffix.json", documentID: "document.json"},
	}

	for _, tc := range cases {
		t.Run(tc.datasetID+"/"+tc.documentID, func(t *testing.T) {
			store := NewWorkspaceStore(newTestWorkspace())
			ws := store.ws

			put, err := store.Put(ctx, PutRequest{
				Document: Document{
					DatasetID: tc.datasetID,
					ID:        tc.documentID,
					Content:   "path-safe",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: tc.datasetID, ID: "sibling-doc", Content: "same dataset"}}); err != nil {
				t.Fatal(err)
			}
			sentinel, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "sentinel-dataset", ID: "sentinel-doc", Content: "keep me"}})
			if err != nil {
				t.Fatal(err)
			}

			datasetSegment := pathSegment(tc.datasetID)
			documentSegment := pathSegment(tc.documentID)
			assertSafeWorkspaceSegment(t, datasetSegment, tc.datasetID)
			assertSafeWorkspaceSegment(t, documentSegment, tc.documentID)

			encodedPath := "datasets/" + datasetSegment + "/documents/" + documentSegment + ".json"
			if exists, err := ws.Exists(ctx, encodedPath); err != nil || !exists {
				t.Fatalf("encoded document exists = %v err %v, want true nil", exists, err)
			}
			rawPath := path.Join("datasets", tc.datasetID, "documents", tc.documentID+".json")
			if rawPath != encodedPath {
				if exists, err := ws.Exists(ctx, rawPath); err != nil || exists {
					t.Fatalf("raw document path %q exists = %v err %v, want false nil", rawPath, exists, err)
				}
			}

			got, ok, err := store.Get(ctx, tc.datasetID, tc.documentID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("Get with encoded ids ok = false, want true")
			}
			if got.ID != put.ID || got.DatasetID != put.DatasetID || got.Content != "path-safe" {
				t.Fatalf("Get = (%q, %q, %q), want original ids and content", got.DatasetID, got.ID, got.Content)
			}

			listed, err := store.List(ctx, tc.datasetID, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertDocumentIDPresent(t, listed, tc.documentID)
			assertDocumentIDPresent(t, listed, "sibling-doc")
			assertDocumentIDAbsent(t, listed, documentSegment)

			datasets, err := store.ListDatasets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertStringPresent(t, datasets, tc.datasetID)
			assertStringPresent(t, datasets, "sentinel-dataset")
			assertStringAbsent(t, datasets, datasetSegment)

			if err := store.Delete(ctx, tc.datasetID, tc.documentID); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := store.Get(ctx, tc.datasetID, tc.documentID); err != nil || ok {
				t.Fatalf("Get after Delete = ok %v err %v, want ok false nil err", ok, err)
			}
			sibling, ok, err := store.Get(ctx, tc.datasetID, "sibling-doc")
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("sibling document after Delete(%q, %q) ok = false, want true", tc.datasetID, tc.documentID)
			}
			if sibling.Content != "same dataset" {
				t.Fatalf("sibling document content after Delete(%q, %q) = %q, want same dataset", tc.datasetID, tc.documentID, sibling.Content)
			}

			if err := store.DeleteDataset(ctx, tc.datasetID); err != nil {
				t.Fatal(err)
			}
			if listed, err := store.List(ctx, tc.datasetID, ListOptions{}); err != nil || len(listed) != 0 {
				t.Fatalf("List after DeleteDataset returned %d docs err %v, want 0 nil", len(listed), err)
			}
			kept, ok, err := store.Get(ctx, "sentinel-dataset", sentinel.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("sentinel dataset after DeleteDataset(%q) ok = false, want true", tc.datasetID)
			}
			if kept.Content != "keep me" {
				t.Fatalf("sentinel dataset content after DeleteDataset(%q) = %q, want keep me", tc.datasetID, kept.Content)
			}

			datasets, err = store.ListDatasets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertStrings(t, datasets, []string{"sentinel-dataset"})
		})
	}
}

func TestWorkspaceStore_MalformedCanonicalJSONReturnsErrors(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace()
	store := NewWorkspaceStore(ws)
	malformedPath := "datasets/" + pathSegment("dataset-1") + "/documents/" + pathSegment("doc-1") + ".json"
	if err := ws.Write(ctx, malformedPath, []byte(`{"dataset_id":`)); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := store.Get(ctx, "dataset-1", "doc-1"); err == nil || ok {
		t.Fatalf("Get malformed document = ok %v err %v, want error", ok, err)
	}
	if _, err := store.List(ctx, "dataset-1", ListOptions{}); err == nil {
		t.Fatal("List malformed dataset err = nil, want error")
	}
	if _, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: "doc-1"}}); err == nil {
		t.Fatal("Put over malformed document err = nil, want error")
	}
}

func TestWorkspaceStore_MissingDirectoriesOnLocalWorkspaceAreEmptyOrIdempotent(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewWorkspaceStore(ws)

	datasets, err := store.ListDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(datasets) != 0 {
		t.Fatalf("ListDatasets on missing datasets dir = %v, want empty", datasets)
	}

	listed, err := store.List(ctx, "missing-dataset", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("List missing dataset returned %d documents, want 0", len(listed))
	}
	if err := store.Delete(ctx, "missing-dataset", "missing-doc"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDataset(ctx, "missing-dataset"); err != nil {
		t.Fatal(err)
	}

	notFoundRemoveAll := NewWorkspaceStore(&removeAllNotFoundWorkspace{Workspace: workspace.NewMemWorkspace()})
	if err := notFoundRemoveAll.DeleteDataset(ctx, "missing-dataset"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceStore_WriteTempPathsAreUniqueAcrossStoreInstances(t *testing.T) {
	ctx := context.Background()
	ws := &recordingWorkspace{Workspace: workspace.NewMemWorkspace()}
	first := NewWorkspaceStore(ws)
	second := NewWorkspaceStore(ws)

	if _, err := first.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: "doc-1", Content: "first"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: "doc-1", Content: "second"}}); err != nil {
		t.Fatal(err)
	}

	tmpWrites := ws.tmpWritePaths()
	if len(tmpWrites) != 2 {
		t.Fatalf("tmp writes = %v, want exactly two", tmpWrites)
	}
	if tmpWrites[0] == tmpWrites[1] {
		t.Fatalf("tmp write paths are identical: %q", tmpWrites[0])
	}
}

func TestWorkspaceStore_ListOrderingAfterIDAndLimit(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	for _, id := range []string{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: id}}); err != nil {
			t.Fatal(err)
		}
	}

	empty, err := store.List(ctx, "missing-dataset", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("List missing dataset returned %d documents, want 0", len(empty))
	}

	all, err := store.List(ctx, "dataset-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, all, []string{"alpha", "bravo", "charlie", "delta"})

	paged, err := store.List(ctx, "dataset-1", ListOptions{AfterID: "bravo", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, paged, []string{"charlie", "delta"})

	unlimited, err := store.List(ctx, "dataset-1", ListOptions{AfterID: "alpha", Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentIDs(t, unlimited, []string{"bravo", "charlie", "delta"})
}

func TestWorkspaceStore_DeleteAndListDatasets(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	for _, doc := range []Document{
		{DatasetID: "dataset-b", ID: "doc-1"},
		{DatasetID: "dataset-a", ID: "doc-1"},
		{DatasetID: "dataset-c", ID: "doc-1"},
	} {
		if _, err := store.Put(ctx, PutRequest{Document: doc}); err != nil {
			t.Fatal(err)
		}
	}

	datasets, err := store.ListDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, datasets, []string{"dataset-a", "dataset-b", "dataset-c"})

	if err := store.Delete(ctx, "dataset-a", "doc-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "dataset-a", "doc-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "dataset-a", "doc-1"); err != nil || ok {
		t.Fatalf("Get after Delete = ok %v err %v, want ok false nil err", ok, err)
	}

	datasets, err = store.ListDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, datasets, []string{"dataset-b", "dataset-c"})

	if err := store.DeleteDataset(ctx, "dataset-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDataset(ctx, "dataset-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDataset(ctx, "missing-dataset"); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.List(ctx, "dataset-b", ListOptions{}); err != nil || len(listed) != 0 {
		t.Fatalf("List after DeleteDataset = %d docs err %v, want 0 nil err", len(listed), err)
	}

	datasets, err = store.ListDatasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, datasets, []string{"dataset-c"})
}

func TestWorkspaceStore_ConcurrentPutsIncrementFinalVersion(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	const count = 64

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, count)
	versions := make(chan uint64, count)
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			doc, err := store.Put(ctx, PutRequest{
				Document: Document{
					DatasetID: "dataset-1",
					ID:        "doc-1",
					Content:   fmt.Sprintf("content-%02d", i),
				},
			})
			if err != nil {
				errs <- err
				return
			}
			versions <- doc.Version
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(versions)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seenVersions := make(map[uint64]struct{}, count)
	for version := range versions {
		seenVersions[version] = struct{}{}
	}
	for i := 1; i <= count; i++ {
		if _, ok := seenVersions[uint64(i)]; !ok {
			t.Fatalf("missing assigned Version %d from concurrent Put results", i)
		}
	}

	got, ok, err := store.Get(ctx, "dataset-1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if got.Version != count {
		t.Fatalf("final Version = %d, want %d", got.Version, count)
	}
}

func TestWorkspaceStore_DeleteDatasetWaitsForInFlightWrite(t *testing.T) {
	ctx := context.Background()
	ws := &blockingWorkspace{
		Workspace:        workspace.NewMemWorkspace(),
		writeStarted:     make(chan struct{}),
		releaseWrite:     make(chan struct{}),
		removeAllEntered: make(chan struct{}),
	}
	store := NewWorkspaceStore(ws)

	putDone := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, PutRequest{Document: Document{DatasetID: "dataset-1", ID: "doc-1", Content: "content"}})
		putDone <- err
	}()

	<-ws.writeStarted
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- store.DeleteDataset(ctx, "dataset-1")
	}()

	deleteOverlapped := false
	select {
	case <-ws.removeAllEntered:
		deleteOverlapped = true
	case <-time.After(50 * time.Millisecond):
	}
	close(ws.releaseWrite)

	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if deleteOverlapped {
		t.Fatal("DeleteDataset entered workspace RemoveAll while Put was still writing")
	}
}

type recordingWorkspace struct {
	workspace.Workspace

	mu         sync.Mutex
	writePaths []string
}

func (w *recordingWorkspace) Write(ctx context.Context, p string, data []byte) error {
	w.mu.Lock()
	w.writePaths = append(w.writePaths, p)
	w.mu.Unlock()
	return w.Workspace.Write(ctx, p, data)
}

func (w *recordingWorkspace) tmpWritePaths() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []string
	for _, p := range w.writePaths {
		if strings.Contains(p, ".tmp.") {
			out = append(out, p)
		}
	}
	return out
}

type blockingWorkspace struct {
	workspace.Workspace

	blockOnce        sync.Once
	writeStarted     chan struct{}
	releaseWrite     chan struct{}
	removeAllEntered chan struct{}
	removeAllOnce    sync.Once
}

func (w *blockingWorkspace) Write(ctx context.Context, p string, data []byte) error {
	if strings.Contains(p, ".tmp.") {
		w.blockOnce.Do(func() {
			close(w.writeStarted)
			<-w.releaseWrite
		})
	}
	return w.Workspace.Write(ctx, p, data)
}

func (w *blockingWorkspace) RemoveAll(ctx context.Context, p string) error {
	w.removeAllOnce.Do(func() {
		close(w.removeAllEntered)
	})
	return w.Workspace.RemoveAll(ctx, p)
}

type removeAllNotFoundWorkspace struct {
	workspace.Workspace
}

func (w *removeAllNotFoundWorkspace) RemoveAll(context.Context, string) error {
	return fmt.Errorf("%w: missing", workspace.ErrNotFound)
}

func assertDocumentIDs(t *testing.T, docs []Document, want []string) {
	t.Helper()
	got := make([]string, 0, len(docs))
	for _, doc := range docs {
		got = append(got, doc.ID)
	}
	assertStrings(t, got, want)
}

func assertDocumentIDPresent(t *testing.T, docs []Document, want string) {
	t.Helper()
	for _, doc := range docs {
		if doc.ID == want {
			return
		}
	}
	t.Fatalf("document id %q not found in %+v", want, docs)
}

func assertDocumentIDAbsent(t *testing.T, docs []Document, unwanted string) {
	t.Helper()
	for _, doc := range docs {
		if doc.ID == unwanted {
			t.Fatalf("document id %q unexpectedly found in %+v", unwanted, docs)
		}
	}
}

func assertStringPresent(t *testing.T, got []string, want string) {
	t.Helper()
	for _, item := range got {
		if item == want {
			return
		}
	}
	t.Fatalf("%q not found in %v", want, got)
}

func assertStringAbsent(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, item := range got {
		if item == unwanted {
			t.Fatalf("%q unexpectedly found in %v", unwanted, got)
		}
	}
}

func assertSafeWorkspaceSegment(t *testing.T, segment, raw string) {
	t.Helper()
	if segment == "" {
		t.Fatal("encoded path segment is empty")
	}
	if segment == "." || segment == ".." {
		t.Fatalf("encoded path segment = %q, want non-special segment", segment)
	}
	if strings.Contains(segment, "/") {
		t.Fatalf("encoded path segment %q contains slash", segment)
	}
	if segment == raw {
		t.Fatalf("encoded path segment = raw id %q", raw)
	}
}

func assertStrings(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(%v) = %d, want %d (%v)", got, len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items = %v, want %v", got, want)
		}
	}
}

func wantContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}
