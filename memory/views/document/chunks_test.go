package document

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestChunksDescriptorDefaultsAndOptions(t *testing.T) {
	defaultView := NewChunks(nil)
	if got := defaultView.Descriptor(); got != (views.Descriptor{ID: DefaultChunksID, Kind: views.KindDocumentChunks, Version: DefaultChunksVersion}) {
		t.Fatalf("default Descriptor = %#v", got)
	}
	if err := defaultView.Descriptor().Validate(); err != nil {
		t.Fatalf("default Descriptor Validate() error = %v", err)
	}

	custom := NewChunks(nil, WithID("custom-chunks"), WithVersion("v-test"))
	if got := custom.Descriptor(); got != (views.Descriptor{ID: "custom-chunks", Kind: views.KindDocumentChunks, Version: "v-test"}) {
		t.Fatalf("custom Descriptor = %#v", got)
	}
}

func TestChunksNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	chunks := NewChunks(nil)
	chunk := validChunk("chunk-1")
	scope := testScope("dataset-1")

	if _, err := chunks.PutChunk(ctx, chunk); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutChunk nil store error = %v, want validation", err)
	}
	if _, _, err := chunks.GetChunk(ctx, scope, "doc-1", "chunk-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetChunk nil store error = %v, want validation", err)
	}
	if _, err := chunks.ListChunks(ctx, "doc-1", ListOptions{Scope: &scope}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListChunks nil store error = %v, want validation", err)
	}
	if err := chunks.DeleteDocument(ctx, scope, "doc-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteDocument nil store error = %v, want validation", err)
	}
	if err := chunks.DeleteDataset(ctx, scope); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteDataset nil store error = %v, want validation", err)
	}
}

func TestValidateChunkCatchesInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Chunk)
	}{
		{name: "missing id", mutate: func(c *Chunk) { c.ID = "" }},
		{name: "missing dataset", mutate: func(c *Chunk) { c.Scope.DatasetID = "" }},
		{name: "missing document", mutate: func(c *Chunk) { c.DocumentID = "" }},
		{name: "missing layer name", mutate: func(c *Chunk) { c.Layer.Name = "" }},
		{name: "missing layer version", mutate: func(c *Chunk) { c.Layer.Version = "" }},
		{name: "missing text", mutate: func(c *Chunk) { c.Text = "" }},
		{name: "negative ordinal", mutate: func(c *Chunk) { c.Ordinal = -1 }},
		{name: "invalid span", mutate: func(c *Chunk) { c.Span = views.Span{Start: 4, End: 3} }},
		{name: "source ref dataset mismatch", mutate: func(c *Chunk) { c.SourceRef.Document.DatasetID = "other-dataset" }},
		{name: "source ref document mismatch", mutate: func(c *Chunk) { c.SourceRef.Document.DocumentID = "other-doc" }},
		{name: "source ref span mismatch", mutate: func(c *Chunk) { c.SourceRef.Document.Span.Start = 1 }},
		{name: "non-document source ref", mutate: func(c *Chunk) {
			c.SourceRef = views.SourceRef{
				Kind:    views.SourceMessage,
				Message: &views.MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1"},
			}
		}},
		{name: "missing document source revisions", mutate: func(c *Chunk) { c.Signature.SourceRevisions = nil }},
		{name: "non-document source revision", mutate: func(c *Chunk) {
			c.Signature.SourceRevisions = []views.SourceRevision{{Kind: views.SourceMessage, SourceKey: "message-key", Revision: "1"}}
		}},
		{name: "upstream refs present", mutate: func(c *Chunk) {
			c.Signature.UpstreamViewRefs = []views.UpstreamViewRef{{ViewID: "other-view", OutputSignature: "out:v1"}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk := validChunk("chunk-1")
			tt.mutate(&chunk)
			if err := chunk.Validate(); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Validate() error = %v, want validation", err)
			}
		})
	}
}

func TestChunksDelegatesAndClones(t *testing.T) {
	ctx := context.Background()
	putResult := validChunk("put-result")
	getResult := validChunk("get-result")
	listResult := validChunk("list-result")
	store := &fakeChunkStore{
		putResult:  putResult,
		getResult:  getResult,
		getOK:      true,
		listResult: []Chunk{listResult},
	}
	view := NewChunks(store)

	input := validChunk("input")
	put, err := view.PutChunk(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("PutChunk calls = %d, want 1", len(store.putCalls))
	}
	mutateChunkNested(&input, "input-mutated")
	assertChunkNestedValue(t, store.putCalls[0], "original")
	mutateChunkNested(&put, "put-mutated")
	assertChunkNestedValue(t, store.putResult, "original")

	scope := testScope("dataset-1")
	got, ok, err := view.GetChunk(ctx, scope, "doc-1", "get-result")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetChunk ok = false, want true")
	}
	if store.getScope != scope || store.getDocumentID != "doc-1" || store.getID != "get-result" {
		t.Fatalf("GetChunk args = %+v/%q/%q", store.getScope, store.getDocumentID, store.getID)
	}
	mutateChunkNested(&got, "get-mutated")
	assertChunkNestedValue(t, store.getResult, "original")

	filter := testLayer()
	opts := ListOptions{AfterID: "after", Limit: 2, Layer: &filter, Scope: &scope}
	listed, err := view.ListChunks(ctx, "doc-1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.listCalls) != 1 {
		t.Fatalf("ListChunks calls = %d, want 1", len(store.listCalls))
	}
	if store.listCalls[0].documentID != "doc-1" {
		t.Fatalf("ListChunks args = %+v", store.listCalls[0])
	}
	if store.listCalls[0].opts.AfterID != "after" || store.listCalls[0].opts.Limit != 2 || store.listCalls[0].opts.Scope == nil || *store.listCalls[0].opts.Scope != scope || store.listCalls[0].opts.Layer == nil || *store.listCalls[0].opts.Layer != filter {
		t.Fatalf("ListChunks opts = %+v", store.listCalls[0].opts)
	}
	filter.Name = "mutated-filter"
	if store.listCalls[0].opts.Layer.Name != testLayer().Name {
		t.Fatalf("ListChunks opts layer shared caller pointer: %+v", store.listCalls[0].opts.Layer)
	}
	mutateChunkNested(&listed[0], "list-mutated")
	assertChunkNestedValue(t, store.listResult[0], "original")

	if err := view.DeleteDocument(ctx, scope, "doc-1"); err != nil {
		t.Fatal(err)
	}
	if store.deleteDocumentScope != scope || store.deleteDocumentID != "doc-1" {
		t.Fatalf("DeleteDocument args = %+v/%q", store.deleteDocumentScope, store.deleteDocumentID)
	}
	if err := view.DeleteDataset(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if store.deleteDatasetScope != scope {
		t.Fatalf("DeleteDataset arg = %+v", store.deleteDatasetScope)
	}
}

func TestChunksListChunksPassesClonedLayerFilter(t *testing.T) {
	ctx := context.Background()
	layer := testLayer()
	scope := testScope("dataset-1")
	opts := ListOptions{AfterID: "chunk-1", Limit: 10, Layer: &layer, Scope: &scope}
	store := &fakeChunkStore{listResult: []Chunk{validChunk("chunk-2")}}
	view := NewChunks(store)

	if _, err := view.ListChunks(ctx, "doc-1", opts); err != nil {
		t.Fatalf("ListChunks error = %v", err)
	}
	if len(store.listCalls) != 1 {
		t.Fatalf("ListChunks calls = %d, want 1", len(store.listCalls))
	}
	got := store.listCalls[0].opts
	if got.AfterID != "chunk-1" || got.Limit != 10 || got.Layer == nil || *got.Layer != layer {
		t.Fatalf("ListChunks opts = %+v, want cloned layer filter %+v", got, layer)
	}
	if got.Layer == opts.Layer {
		t.Fatal("ListChunks shared caller Layer pointer with store options")
	}
	if got.Scope == opts.Scope {
		t.Fatal("ListChunks shared caller Scope pointer with store options")
	}
	layer.Name = "mutated"
	if store.listCalls[0].opts.Layer.Name != testLayer().Name {
		t.Fatalf("ListChunks opts layer shared caller state: %+v", store.listCalls[0].opts.Layer)
	}
}

func TestChunksDeleteDocumentAndDatasetAreNotLayerScoped(t *testing.T) {
	ctx := context.Background()
	store := &fakeChunkStore{}
	view := NewChunks(store)
	scope := testScope("dataset-1")

	if err := view.DeleteDocument(ctx, scope, "doc-1"); err != nil {
		t.Fatal(err)
	}
	if store.deleteDocumentScope != scope || store.deleteDocumentID != "doc-1" {
		t.Fatalf("DeleteDocument args = %+v/%q", store.deleteDocumentScope, store.deleteDocumentID)
	}
	if err := view.DeleteDataset(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if store.deleteDatasetScope != scope {
		t.Fatalf("DeleteDataset arg = %+v", store.deleteDatasetScope)
	}
}

func TestCloneChunkClonesNestedState(t *testing.T) {
	original := validChunk("chunk-1")
	cloned := cloneChunk(original)

	mutateChunkNested(&cloned, "mutated")
	assertChunkNestedValue(t, original, "original")
}

func validChunk(id ChunkID) Chunk {
	span := views.Span{Start: 0, End: 11}
	sourceRef := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:   "dataset-1",
			DocumentID:  "doc-1",
			Version:     "2",
			ContentHash: "sha256:document",
			Span:        &span,
		},
	}
	return Chunk{
		ID:         id,
		Scope:      testScope("dataset-1"),
		DocumentID: "doc-1",
		Layer:      testLayer(),
		Ordinal:    1,
		Span:       span,
		Text:       "hello world",
		SourceRef:  sourceRef,
		Signature: views.ViewSignature{
			ViewID:             DefaultChunksID,
			TransformSignature: testLayer().TransformSignature,
			SourceRevisions: []views.SourceRevision{{
				Kind:        views.SourceDocument,
				SourceKey:   sourceRef.StableKey(),
				Revision:    "2",
				ContentHash: "sha256:document",
			}},
			DiagnosticSignatures: map[string]string{"chunker": "paragraph:v1"},
		},
		CreatedAt: time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC),
		Metadata: map[string]any{
			"key":    "original",
			"nested": map[string]any{"key": "original"},
			"array":  []any{map[string]any{"key": "original"}},
		},
	}
}

func testScope(datasetID string) views.Scope {
	return views.Scope{RuntimeID: "runtime-1", UserID: "user-1", DatasetID: datasetID}
}

func scopePtr(scope views.Scope) *views.Scope {
	return &scope
}

func testLayer() Layer {
	return Layer{Name: "paragraph", Version: "v1", TransformSignature: "paragraph:v1"}
}

func mutateChunkNested(chunk *Chunk, value string) {
	chunk.SourceRef.Document.Span.Start = 99
	chunk.Signature.SourceRevisions[0].Revision = value
	chunk.Signature.DiagnosticSignatures["chunker"] = value
	chunk.Metadata["key"] = value
	chunk.Metadata["nested"].(map[string]any)["key"] = value
	chunk.Metadata["array"].([]any)[0].(map[string]any)["key"] = value
}

func assertChunkNestedValue(t *testing.T, chunk Chunk, want string) {
	t.Helper()
	if chunk.SourceRef.Document.Span.Start != 0 {
		t.Fatalf("source ref span start = %d, want 0", chunk.SourceRef.Document.Span.Start)
	}
	if chunk.Signature.SourceRevisions[0].Revision != "2" {
		t.Fatalf("source revision = %q, want 2", chunk.Signature.SourceRevisions[0].Revision)
	}
	if chunk.Signature.DiagnosticSignatures["chunker"] != "paragraph:v1" {
		t.Fatalf("diagnostic signature = %q, want paragraph:v1", chunk.Signature.DiagnosticSignatures["chunker"])
	}
	if chunk.Metadata["key"] != want {
		t.Fatalf("metadata key = %q, want %q", chunk.Metadata["key"], want)
	}
	if chunk.Metadata["nested"].(map[string]any)["key"] != want {
		t.Fatalf("metadata nested key = %q, want %q", chunk.Metadata["nested"].(map[string]any)["key"], want)
	}
	if chunk.Metadata["array"].([]any)[0].(map[string]any)["key"] != want {
		t.Fatalf("metadata array key = %q, want %q", chunk.Metadata["array"].([]any)[0].(map[string]any)["key"], want)
	}
}

type fakeChunkStore struct {
	putCalls  []Chunk
	putResult Chunk

	getScope      views.Scope
	getDocumentID string
	getID         ChunkID
	getResult     Chunk
	getOK         bool

	listCalls  []fakeListCall
	listResult []Chunk

	deleteDocumentScope     views.Scope
	deleteDocumentID        string
	deleteDatasetScope      views.Scope
}

type fakeListCall struct {
	documentID string
	opts       ListOptions
}

func (s *fakeChunkStore) PutChunk(_ context.Context, chunk Chunk) (Chunk, error) {
	s.putCalls = append(s.putCalls, chunk)
	return s.putResult, nil
}

func (s *fakeChunkStore) GetChunk(_ context.Context, scope views.Scope, documentID string, id ChunkID) (Chunk, bool, error) {
	s.getScope = scope
	s.getDocumentID = documentID
	s.getID = id
	return s.getResult, s.getOK, nil
}

func (s *fakeChunkStore) ListChunks(_ context.Context, documentID string, opts ListOptions) ([]Chunk, error) {
	s.listCalls = append(s.listCalls, fakeListCall{documentID: documentID, opts: opts})
	return s.listResult, nil
}

func (s *fakeChunkStore) DeleteDocument(_ context.Context, scope views.Scope, documentID string) error {
	s.deleteDocumentScope = scope
	s.deleteDocumentID = documentID
	return nil
}

func (s *fakeChunkStore) DeleteDataset(_ context.Context, scope views.Scope) error {
	s.deleteDatasetScope = scope
	return nil
}

func TestValidChunkHelperIsValid(t *testing.T) {
	chunk := validChunk("chunk-1")
	if err := chunk.Validate(); err != nil {
		t.Fatalf("validChunk Validate() error = %v", err)
	}
	if !reflect.DeepEqual(cloneChunk(chunk), chunk) {
		t.Fatal("cloneChunk changed valid chunk value")
	}
}
