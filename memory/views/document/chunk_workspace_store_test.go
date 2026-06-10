package document

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestChunkWorkspaceStoreNilWorkspaceReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(nil)

	if _, err := store.PutChunk(ctx, validChunk("chunk-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutChunk nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetChunk nil workspace error = %v, want validation", err)
	}
	if _, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListChunks nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteDocument(ctx, "dataset-1", "doc-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteDocument nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteDataset(ctx, "dataset-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteDataset nil workspace error = %v, want validation", err)
	}
}

func TestChunkWorkspaceStorePutGetDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())
	chunk := validChunk("chunk-1")

	put, err := store.PutChunk(ctx, chunk)
	if err != nil {
		t.Fatal(err)
	}
	assertChunkEqual(t, put, chunk)

	mutateChunkNested(&chunk, "input-mutated")
	mutateChunkNested(&put, "put-mutated")

	got, ok, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk ok = false, want true")
	}
	assertChunkNestedValue(t, got, "original")

	mutateChunkNested(&got, "get-mutated")
	again, ok, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk after mutation ok = false, want true")
	}
	assertChunkNestedValue(t, again, "original")

	listed, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listed[0].Metadata["key"] = "list-mutated"
	again, ok, err = store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk after list mutation ok = false, want true")
	}
	assertChunkNestedValue(t, again, "original")
}

func TestChunkWorkspaceStoreListOrderAfterIDLimitAndLayerFilter(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())
	layerA := testLayer()
	layerB := Layer{Name: "semantic", Version: "v2", TransformSignature: "semantic:v2"}
	layers := map[ChunkID]Layer{
		"bravo":   layerA,
		"alpha":   layerA,
		"delta":   layerB,
		"charlie": layerA,
	}

	for _, id := range []ChunkID{"bravo", "alpha", "delta", "charlie"} {
		chunk := validChunk(id)
		chunk.Layer = layers[id]
		chunk.Signature.TransformSignature = layers[id].TransformSignature
		if _, err := store.PutChunk(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertChunkIDs(t, all, []ChunkID{"alpha", "bravo", "charlie", "delta"})

	filtered, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{AfterID: "alpha", Limit: 2, Layer: &layerA})
	if err != nil {
		t.Fatal(err)
	}
	assertChunkIDs(t, filtered, []ChunkID{"bravo", "charlie"})

	onlyLayerB, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{Layer: &layerB})
	if err != nil {
		t.Fatal(err)
	}
	assertChunkIDs(t, onlyLayerB, []ChunkID{"delta"})

	missing, err := store.ListChunks(ctx, "dataset-1", "missing-doc", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("ListChunks missing document returned %d chunks, want 0", len(missing))
	}
}

func TestChunkWorkspaceStorePutReplacesSameIDAcrossLayers(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())
	layerA := testLayer()
	layerB := Layer{Name: "semantic", Version: "v2", TransformSignature: "semantic:v2"}

	first := chunkWithScope("dataset-1", "doc-1", layerA, "chunk-1")
	if _, err := store.PutChunk(ctx, first); err != nil {
		t.Fatal(err)
	}
	replacement := chunkWithScope("dataset-1", "doc-1", layerB, "chunk-1")
	replacement.Text = "replacement text"
	if _, err := store.PutChunk(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk replacement ok = false, want true")
	}
	if got.Layer != layerB || got.Text != "replacement text" {
		t.Fatalf("GetChunk replacement = %+v, want layerB replacement", got)
	}
	oldLayer, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{Layer: &layerA})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldLayer) != 0 {
		t.Fatalf("ListChunks old layer returned %d chunks, want 0", len(oldLayer))
	}
	newLayer, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{Layer: &layerB})
	if err != nil {
		t.Fatal(err)
	}
	assertChunkIDs(t, newLayer, []ChunkID{"chunk-1"})
}

func TestChunkWorkspaceStorePutAcrossLayersKeepsOldChunkWhenPublishFails(t *testing.T) {
	ctx := context.Background()
	ws := &failRenameWorkspace{Workspace: workspace.NewMemWorkspace()}
	store := NewChunkWorkspaceStore(ws)
	layerA := testLayer()
	layerB := Layer{Name: "semantic", Version: "v2", TransformSignature: "semantic:v2"}

	first := chunkWithScope("dataset-1", "doc-1", layerA, "chunk-1")
	first.Text = "old text"
	if _, err := store.PutChunk(ctx, first); err != nil {
		t.Fatal(err)
	}

	replacement := chunkWithScope("dataset-1", "doc-1", layerB, "chunk-1")
	replacement.Text = "replacement text"
	ws.failRenameDst = store.chunkPath(replacement.DatasetID, replacement.DocumentID, replacement.Layer, replacement.ID)

	if _, err := store.PutChunk(ctx, replacement); err == nil || !errors.Is(err, errFailRename) {
		t.Fatalf("PutChunk replacement error = %v, want fail rename", err)
	}

	got, ok, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk after failed replacement ok = false, want old chunk")
	}
	if got.Layer != layerA || got.Text != "old text" {
		t.Fatalf("GetChunk after failed replacement = %+v, want old layer chunk", got)
	}
}

func TestChunkWorkspaceStoreDeleteDocumentRemovesAllLayers(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())
	layerB := Layer{Name: "semantic", Version: "v2", TransformSignature: "semantic:v2"}

	for _, chunk := range []Chunk{
		validChunk("chunk-a"),
		chunkWithScope("dataset-1", "doc-1", layerB, "chunk-b"),
		chunkWithScope("dataset-1", "doc-2", testLayer(), "kept-doc"),
		chunkWithScope("dataset-2", "doc-1", testLayer(), "kept-dataset"),
	} {
		if _, err := store.PutChunk(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteDocument(ctx, "dataset-1", "doc-1"); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.ListChunks(ctx, "dataset-1", "doc-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("ListChunks deleted document returned %d chunks, want 0", len(deleted))
	}

	keptDoc, ok, err := store.GetChunk(ctx, "dataset-1", "doc-2", "kept-doc")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || keptDoc.DocumentID != "doc-2" {
		t.Fatalf("GetChunk kept document = %+v ok %v, want doc-2 true", keptDoc, ok)
	}
	keptDataset, ok, err := store.GetChunk(ctx, "dataset-2", "doc-1", "kept-dataset")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || keptDataset.DatasetID != "dataset-2" {
		t.Fatalf("GetChunk kept dataset = %+v ok %v, want dataset-2 true", keptDataset, ok)
	}
}

func TestChunkWorkspaceStoreDeleteDatasetRemovesAllDocuments(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())

	for _, chunk := range []Chunk{
		chunkWithScope("dataset-1", "doc-1", testLayer(), "chunk-1"),
		chunkWithScope("dataset-1", "doc-2", testLayer(), "chunk-2"),
		chunkWithScope("dataset-2", "doc-1", testLayer(), "kept"),
	} {
		if _, err := store.PutChunk(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteDataset(ctx, "dataset-1"); err != nil {
		t.Fatal(err)
	}
	for _, docID := range []string{"doc-1", "doc-2"} {
		deleted, err := store.ListChunks(ctx, "dataset-1", docID, ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 0 {
			t.Fatalf("ListChunks deleted dataset document %q returned %d chunks, want 0", docID, len(deleted))
		}
	}
	kept, ok, err := store.GetChunk(ctx, "dataset-2", "doc-1", "kept")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || kept.DatasetID != "dataset-2" {
		t.Fatalf("GetChunk kept dataset = %+v ok %v, want dataset-2 true", kept, ok)
	}
}

func TestChunkWorkspaceStorePathSegmentPrefixDefaultCustomAndEmpty(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name       string
		prefix     string
		wantPrefix string
		opts       []ChunkWorkspaceStoreOption
	}{
		{name: "default", wantPrefix: "chunk_"},
		{name: "custom", prefix: "custom_", wantPrefix: "custom_", opts: []ChunkWorkspaceStoreOption{WithChunkPathSegmentPrefix("custom_")}},
		{name: "empty", prefix: "", wantPrefix: "", opts: []ChunkWorkspaceStoreOption{WithChunkPathSegmentPrefix("")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ws := workspace.NewMemWorkspace()
			store := NewChunkWorkspaceStore(ws, tt.opts...)
			layer := Layer{Name: "layer/with/slash", Version: "v1", TransformSignature: "transform/with/slash"}
			chunk := chunkWithScope("dataset/with/slash", "doc/with/slash", layer, "chunk/with/slash")

			if _, err := store.PutChunk(ctx, chunk); err != nil {
				t.Fatal(err)
			}

			datasetSegment := store.pathSegment(chunk.DatasetID)
			documentSegment := store.pathSegment(chunk.DocumentID)
			layerSegment := store.pathSegment(layerIdentity(chunk.Layer))
			chunkSegment := store.pathSegment(string(chunk.ID))
			assertSafeChunkWorkspaceSegment(t, store, datasetSegment, chunk.DatasetID, tt.wantPrefix)
			assertSafeChunkWorkspaceSegment(t, store, documentSegment, chunk.DocumentID, tt.wantPrefix)
			assertSafeChunkWorkspaceSegment(t, store, layerSegment, layerIdentity(chunk.Layer), tt.wantPrefix)
			assertSafeChunkWorkspaceSegment(t, store, chunkSegment, string(chunk.ID), tt.wantPrefix)

			encodedPath := "datasets/" + datasetSegment + "/documents/" + documentSegment + "/layers/" + layerSegment + "/chunks/" + chunkSegment + ".json"
			if exists, err := ws.Exists(ctx, encodedPath); err != nil || !exists {
				t.Fatalf("encoded chunk exists = %v err %v, want true nil", exists, err)
			}
			rawPath := "datasets/" + chunk.DatasetID + "/documents/" + chunk.DocumentID + "/layers/" + layerIdentity(chunk.Layer) + "/chunks/" + string(chunk.ID) + ".json"
			if rawPath != encodedPath {
				if exists, err := ws.Exists(ctx, rawPath); err != nil || exists {
					t.Fatalf("raw chunk path %q exists = %v err %v, want false nil", rawPath, exists, err)
				}
			}

			got, ok, err := store.GetChunk(ctx, chunk.DatasetID, chunk.DocumentID, chunk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("GetChunk ok = false, want true")
			}
			assertChunkEqual(t, got, chunk)
		})
	}
}

func TestChunkWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())
	chunk := validChunk("chunk-1")
	chunk.Metadata = map[string]any{
		"int":  7,
		"bool": true,
		"nested": map[string]any{
			"count": 2,
			"ok":    false,
		},
		"array": []any{1, map[string]any{"count": 3}},
	}

	if _, err := store.PutChunk(ctx, chunk); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetChunk(ctx, "dataset-1", "doc-1", "chunk-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk ok = false, want true")
	}

	if got.Metadata["int"] != float64(7) {
		t.Fatalf("metadata int = %#v, want float64(7)", got.Metadata["int"])
	}
	if got.Metadata["bool"] != true {
		t.Fatalf("metadata bool = %#v, want true", got.Metadata["bool"])
	}
	nested, ok := got.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested type = %T, want map[string]any", got.Metadata["nested"])
	}
	if nested["count"] != float64(2) || nested["ok"] != false {
		t.Fatalf("metadata nested = %#v, want count float64(2) and ok false", nested)
	}
	array, ok := got.Metadata["array"].([]any)
	if !ok {
		t.Fatalf("metadata array type = %T, want []any", got.Metadata["array"])
	}
	if array[0] != float64(1) {
		t.Fatalf("metadata array[0] = %#v, want float64(1)", array[0])
	}
	arrayMap, ok := array[1].(map[string]any)
	if !ok {
		t.Fatalf("metadata array[1] type = %T, want map[string]any", array[1])
	}
	if arrayMap["count"] != float64(3) {
		t.Fatalf("metadata array nested count = %#v, want float64(3)", arrayMap["count"])
	}
}

func TestChunkWorkspaceStoreValidationErrors(t *testing.T) {
	ctx := context.Background()
	store := NewChunkWorkspaceStore(workspace.NewMemWorkspace())

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "get missing dataset", run: func() error {
			_, _, err := store.GetChunk(ctx, "", "doc-1", "chunk-1")
			return err
		}},
		{name: "get missing document", run: func() error {
			_, _, err := store.GetChunk(ctx, "dataset-1", "", "chunk-1")
			return err
		}},
		{name: "get missing id", run: func() error {
			_, _, err := store.GetChunk(ctx, "dataset-1", "doc-1", "")
			return err
		}},
		{name: "list missing dataset", run: func() error {
			_, err := store.ListChunks(ctx, "", "doc-1", ListOptions{})
			return err
		}},
		{name: "list missing document", run: func() error {
			_, err := store.ListChunks(ctx, "dataset-1", "", ListOptions{})
			return err
		}},
		{name: "delete document missing dataset", run: func() error {
			return store.DeleteDocument(ctx, "", "doc-1")
		}},
		{name: "delete document missing document", run: func() error {
			return store.DeleteDocument(ctx, "dataset-1", "")
		}},
		{name: "delete dataset missing dataset", run: func() error {
			return store.DeleteDataset(ctx, "")
		}},
		{name: "put invalid chunk", run: func() error {
			chunk := validChunk("chunk-1")
			chunk.ID = ""
			_, err := store.PutChunk(ctx, chunk)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func chunkWithScope(datasetID, documentID string, layer Layer, id ChunkID) Chunk {
	chunk := validChunk(id)
	chunk.DatasetID = datasetID
	chunk.DocumentID = documentID
	chunk.Layer = layer
	chunk.Signature.TransformSignature = layer.TransformSignature
	chunk.SourceRef.Document.DatasetID = datasetID
	chunk.SourceRef.Document.DocumentID = documentID
	chunk.Signature.SourceRevisions[0].SourceKey = chunk.SourceRef.StableKey()
	return chunk
}

func assertChunkEqual(t *testing.T, got, want Chunk) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunk mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertChunkIDs(t *testing.T, chunks []Chunk, want []ChunkID) {
	t.Helper()
	got := make([]ChunkID, 0, len(chunks))
	for _, chunk := range chunks {
		got = append(got, chunk.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chunk IDs = %v, want %v", got, want)
	}
}

func assertSafeChunkWorkspaceSegment(t *testing.T, store *ChunkWorkspaceStore, segment, raw, wantPrefix string) {
	t.Helper()
	if !strings.HasPrefix(segment, wantPrefix) {
		t.Fatalf("segment %q for raw %q missing %q prefix", segment, raw, wantPrefix)
	}
	if strings.Contains(segment, "/") || segment == "." || segment == ".." {
		t.Fatalf("segment %q for raw %q is not path safe", segment, raw)
	}
	decoded, err := store.rawPathSegment(segment)
	if err != nil {
		t.Fatalf("rawPathSegment(%q) error = %v", segment, err)
	}
	if decoded != raw {
		t.Fatalf("rawPathSegment(%q) = %q, want %q", segment, decoded, raw)
	}
}

var errFailRename = errors.New("forced rename failure")

type failRenameWorkspace struct {
	workspace.Workspace
	failRenameDst string
}

func (w *failRenameWorkspace) Rename(ctx context.Context, src, dst string) error {
	if dst == w.failRenameDst {
		return errFailRename
	}
	return w.Workspace.Rename(ctx, src, dst)
}
