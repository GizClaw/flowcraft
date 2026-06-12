package indexed

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	"github.com/GizClaw/flowcraft/memory/views"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const validNamespace = "memory_views_projected_docs"

func TestNewWriterValidatesInputs(t *testing.T) {
	if _, err := NewWriter(nil, validBinding()); err == nil {
		t.Fatal("NewWriter(nil index) error = nil, want error")
	}

	if _, err := NewWriter(&fakeIndex{}, validBinding()); err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}
}

func TestBindingValidatesNamespaceContract(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
	}{
		{name: "empty", namespace: ""},
		{name: "dot", namespace: "memory.views.projected_docs"},
		{name: "dash", namespace: "memory-views-projected-docs"},
		{name: "slash", namespace: "memory/views/projected_docs"},
		{name: "unicode", namespace: "memory_views_投影"},
		{name: "too long", namespace: strings.Repeat("a", 49)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewWriter(&fakeIndex{}, Binding{Namespace: tt.namespace}); err == nil {
				t.Fatal("NewWriter(invalid namespace) error = nil, want error")
			} else if !strings.HasPrefix(err.Error(), errPrefix+": ") {
				t.Fatalf("NewWriter(invalid namespace) error = %q, want %q prefix", err, errPrefix+": ")
			}
		})
	}
}

func TestBindingIsNamespaceOnly(t *testing.T) {
	writer, err := NewWriter(&fakeIndex{}, Binding{Namespace: validNamespace})
	if err != nil {
		t.Fatalf("NewWriter(namespace-only binding) error = %v", err)
	}
	if got := writer.Binding().Namespace; got != validNamespace {
		t.Fatalf("Binding().Namespace = %q, want %s", got, validNamespace)
	}
}

func TestWriterBindingReturnsCopy(t *testing.T) {
	binding := validBinding()
	writer, err := NewWriter(&fakeIndex{}, binding)
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}

	binding.Namespace = "mutated"
	if got := writer.Binding().Namespace; got != validNamespace {
		t.Fatalf("Binding() changed after caller mutation: %q", got)
	}

	gotBinding := writer.Binding()
	gotBinding.Namespace = "mutated-again"
	if got := writer.Binding().Namespace; got != validNamespace {
		t.Fatalf("Binding() returned shared state: %q", got)
	}
}

func TestWriterUpsertConvertsRecordsToDocs(t *testing.T) {
	index := &fakeIndex{}
	writer, err := NewWriter(index, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}

	record := Record{
		ID:       "record-1",
		Text:     "hello indexed projection",
		Vector:   []float32{1, 2},
		Metadata: map[string]any{"kind": "example"},
		SourceRefs: []views.SourceRef{{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: "conv-1",
				MessageID:      "msg-1",
				Span:           &views.Span{Start: 1, End: 3},
			},
		}},
		Signature: views.ViewSignature{
			ViewID:             views.ID("projected-docs"),
			TransformSignature: "record-transform:v1",
			DiagnosticSignatures: map[string]string{
				"embedder": "test-embedder:v1",
			},
		},
	}

	if err := writer.Upsert(context.Background(), []Record{record}); err != nil {
		t.Fatalf("Upsert(valid record) error = %v", err)
	}

	if index.upsertNamespace != validNamespace {
		t.Fatalf("Upsert namespace = %q, want %s", index.upsertNamespace, validNamespace)
	}
	if len(index.upsertDocs) != 1 {
		t.Fatalf("Upsert docs len = %d, want 1", len(index.upsertDocs))
	}
	doc := index.upsertDocs[0]
	if doc.ID != record.ID || doc.Content != record.Text {
		t.Fatalf("Upsert doc = %+v, want id/content from record", doc)
	}
	if len(doc.Vector) != 2 || doc.Vector[0] != 1 || doc.Vector[1] != 2 {
		t.Fatalf("Upsert doc vector = %v, want [1 2]", doc.Vector)
	}
	if doc.Metadata["kind"] != "example" {
		t.Fatalf("Upsert doc metadata kind = %v, want example", doc.Metadata["kind"])
	}
	refs, ok, err := DecodeSourceRefs(doc.Metadata)
	if err != nil {
		t.Fatalf("DecodeSourceRefs() error = %v", err)
	}
	if !ok || len(refs) != 1 || refs[0].Message.MessageID != "msg-1" {
		t.Fatalf("Upsert doc source refs = %#v, want cloned source ref", refs)
	}
	signature, ok, err := DecodeSignature(doc.Metadata)
	if err != nil {
		t.Fatalf("DecodeSignature() error = %v", err)
	}
	if !ok || signature.DiagnosticSignatures["embedder"] != "test-embedder:v1" {
		t.Fatalf("Upsert doc signature = %#v, want cloned signature", signature)
	}

	record.Vector[0] = 99
	record.Metadata["kind"] = "mutated"
	record.SourceRefs[0].Message.MessageID = "mutated"
	record.Signature.DiagnosticSignatures["embedder"] = "mutated"
	if doc.Vector[0] != 1 || doc.Metadata["kind"] != "example" {
		t.Fatalf("Upsert doc shares vector or metadata with record: %+v", doc)
	}
	refs, _, err = DecodeSourceRefs(doc.Metadata)
	if err != nil {
		t.Fatalf("DecodeSourceRefs() after mutation error = %v", err)
	}
	signature, _, err = DecodeSignature(doc.Metadata)
	if err != nil {
		t.Fatalf("DecodeSignature() after mutation error = %v", err)
	}
	if refs[0].Message.MessageID != "msg-1" || signature.DiagnosticSignatures["embedder"] != "test-embedder:v1" {
		t.Fatalf("Upsert doc shares lineage with record: refs=%+v signature=%+v", refs, signature)
	}
}

func TestWriterUpsertVectorizesMissingRecordVectors(t *testing.T) {
	index := &fakeIndex{}
	embedder := &fakeEmbedder{
		batchVectors: [][]float32{
			{10, 11},
			{20, 21},
		},
	}
	writer, err := NewWriter(index, validBinding(), WithEmbedder(embedder), WithVectorize(true))
	if err != nil {
		t.Fatalf("NewWriter(vectorized) error = %v", err)
	}

	records := []Record{
		{ID: "needs-vector-1", Text: "first text"},
		{ID: "keeps-vector", Text: "second text", Vector: []float32{7, 8}},
		{ID: "vector-only", Vector: []float32{9, 9}},
		{ID: "needs-vector-2", Text: "third text"},
	}
	if err := writer.Upsert(context.Background(), records); err != nil {
		t.Fatalf("Upsert(vectorized) error = %v", err)
	}

	wantTexts := []string{"first text", "third text"}
	if !reflect.DeepEqual(embedder.batchTexts, wantTexts) {
		t.Fatalf("EmbedBatch texts = %q, want %q", embedder.batchTexts, wantTexts)
	}
	if got := index.upsertDocs[0].Vector; !reflect.DeepEqual(got, []float32{10, 11}) {
		t.Fatalf("doc[0].Vector = %v, want generated vector", got)
	}
	if got := index.upsertDocs[1].Vector; !reflect.DeepEqual(got, []float32{7, 8}) {
		t.Fatalf("doc[1].Vector = %v, want existing vector preserved", got)
	}
	if got := index.upsertDocs[2].Vector; !reflect.DeepEqual(got, []float32{9, 9}) {
		t.Fatalf("doc[2].Vector = %v, want vector-only record preserved", got)
	}
	if got := index.upsertDocs[3].Vector; !reflect.DeepEqual(got, []float32{20, 21}) {
		t.Fatalf("doc[3].Vector = %v, want second generated vector", got)
	}
	if len(records[0].Vector) != 0 {
		t.Fatalf("Upsert mutated caller record vector: %v", records[0].Vector)
	}
}

func TestWriterUpsertEmbeddingTimeoutReturnsDeadlineExceeded(t *testing.T) {
	index := &fakeIndex{}
	writer, err := NewWriter(
		index,
		validBinding(),
		WithEmbedder(blockingEmbedder{}),
		WithVectorize(true),
		WithEmbeddingTimeout(time.Nanosecond),
	)
	if err != nil {
		t.Fatalf("NewWriter(timeout vectorized) error = %v", err)
	}

	err = writer.Upsert(context.Background(), []Record{{ID: "needs-vector", Text: "slow text"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Upsert(timeout) error = %v, want deadline exceeded", err)
	}
	if len(index.upsertDocs) != 0 {
		t.Fatalf("Upsert(timeout) wrote %d docs, want none", len(index.upsertDocs))
	}
}

func TestWriterUpsertHandlesEmptyBatch(t *testing.T) {
	writer, err := NewWriter(&fakeIndex{}, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}
	if err := writer.Upsert(context.Background(), nil); err != nil {
		t.Fatalf("Upsert(empty) error = %v", err)
	}
}

func TestWriterUpsertMinimalRecordOmitsLineageMetadata(t *testing.T) {
	index := &fakeIndex{}
	writer, err := NewWriter(index, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}

	if err := writer.Upsert(context.Background(), []Record{{ID: "minimal", Text: "hello"}}); err != nil {
		t.Fatalf("Upsert(minimal) error = %v", err)
	}
	if len(index.upsertDocs) != 1 {
		t.Fatalf("Upsert docs len = %d, want 1", len(index.upsertDocs))
	}
	doc := index.upsertDocs[0]
	if _, ok, err := DecodeSourceRefs(doc.Metadata); err != nil || ok {
		t.Fatalf("DecodeSourceRefs(minimal) refs present ok=%v err=%v metadata=%+v", ok, err, doc.Metadata)
	}
	if _, ok, err := DecodeSignature(doc.Metadata); err != nil || ok {
		t.Fatalf("DecodeSignature(minimal) signature present ok=%v err=%v metadata=%+v", ok, err, doc.Metadata)
	}
}

func TestDecodeLineageMetadataAfterJSONRoundTrip(t *testing.T) {
	record := lineageRecord()
	metadata := metadataFromRecord(record)

	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal(metadata) error = %v", err)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("Unmarshal(metadata) error = %v", err)
	}

	assertDecodedLineage(t, roundTripped, record.SourceRefs, record.Signature)
}

func TestWorkspaceRoundTripDecodesLineageMetadata(t *testing.T) {
	ctx := context.Background()
	ws := sdkworkspace.NewMemWorkspace()
	idx, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}

	record := lineageRecord()
	writer, err := NewWriter(idx, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(workspace) error = %v", err)
	}
	if err := writer.Upsert(ctx, []Record{record}); err != nil {
		t.Fatalf("Upsert(workspace) error = %v", err)
	}
	if err := idx.Flush(ctx, validNamespace); err != nil {
		t.Fatalf("Flush(workspace) error = %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close(workspace) error = %v", err)
	}

	reopened, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	getter, ok := retrieval.AsDocGetter(reopened)
	if !ok {
		t.Fatal("workspace index should expose DocGetter")
	}
	doc, ok, err := getter.Get(ctx, validNamespace, record.ID)
	if err != nil {
		t.Fatalf("Get(reopened) error = %v", err)
	}
	if !ok {
		t.Fatal("Get(reopened) ok = false, want true")
	}
	assertDecodedLineage(t, doc.Metadata, record.SourceRefs, record.Signature)

	deleteWriter, err := NewWriter(reopened, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(reopened) error = %v", err)
	}
	if err := deleteWriter.Delete(ctx, []string{record.ID}); err != nil {
		t.Fatalf("Delete(reopened) error = %v", err)
	}
	if err := reopened.Flush(ctx, validNamespace); err != nil {
		t.Fatalf("Flush(delete) error = %v", err)
	}
	if got, ok, err := getter.Get(ctx, validNamespace, record.ID); err != nil || ok {
		t.Fatalf("Get(after delete) got=%+v ok=%v err=%v, want miss", got, ok, err)
	}
}

func TestWriterDeleteUsesBoundNamespace(t *testing.T) {
	index := &fakeIndex{}
	writer, err := NewWriter(index, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}

	if err := writer.Delete(context.Background(), []string{"record-1", "record-2"}); err != nil {
		t.Fatalf("Delete(ids) error = %v", err)
	}
	if index.deleteNamespace != validNamespace {
		t.Fatalf("Delete namespace = %q, want %s", index.deleteNamespace, validNamespace)
	}
	if len(index.deleteIDs) != 2 || index.deleteIDs[0] != "record-1" || index.deleteIDs[1] != "record-2" {
		t.Fatalf("Delete ids = %v, want [record-1 record-2]", index.deleteIDs)
	}
}

func TestWriterDrop(t *testing.T) {
	droppable := &droppableFakeIndex{fakeIndex: &fakeIndex{}}
	writer, err := NewWriter(droppable, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid) error = %v", err)
	}

	if err := writer.Drop(context.Background()); err != nil {
		t.Fatalf("Drop(droppable) error = %v", err)
	}
	if droppable.dropNamespace != validNamespace {
		t.Fatalf("Drop namespace = %q, want %s", droppable.dropNamespace, validNamespace)
	}

	notDroppable, err := NewWriter(&fakeIndex{}, validBinding())
	if err != nil {
		t.Fatalf("NewWriter(valid non-droppable) error = %v", err)
	}
	if err := notDroppable.Drop(context.Background()); err == nil {
		t.Fatal("Drop(non-droppable) error = nil, want error")
	}
}

func TestRecordValidation(t *testing.T) {
	valid := validRecord()
	tests := []struct {
		name   string
		record Record
	}{
		{
			name: "missing id",
			record: func() Record {
				r := valid
				r.ID = ""
				return r
			}(),
		},
		{
			name: "missing text and vector",
			record: func() Record {
				r := valid
				r.Text = ""
				r.Vector = nil
				return r
			}(),
		},
		{
			name: "invalid source ref",
			record: func() Record {
				r := valid
				r.SourceRefs = []views.SourceRef{{Kind: views.SourceMessage, Message: &views.MessageSourceRef{ConversationID: "conv-1"}}}
				return r
			}(),
		},
		{
			name: "invalid signature",
			record: func() Record {
				r := valid
				r.Signature = views.ViewSignature{TransformSignature: "missing-view-id"}
				return r
			}(),
		},
		{
			name: "reserved source refs metadata key",
			record: func() Record {
				r := valid
				r.Metadata = map[string]any{MetadataSourceRefsKey: "caller-value"}
				return r
			}(),
		},
		{
			name: "reserved signature metadata key",
			record: func() Record {
				r := valid
				r.Metadata = map[string]any{MetadataSignatureKey: "caller-value"}
				return r
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.record.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			} else if !strings.HasPrefix(err.Error(), errPrefix+": ") {
				t.Fatalf("Validate() error = %q, want %q prefix", err, errPrefix+": ")
			}
		})
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}
}

func validBinding() Binding {
	return Binding{
		Namespace: validNamespace,
	}
}

func validRecord() Record {
	return Record{
		ID:     "record-1",
		Text:   "hello",
		Vector: []float32{1},
		SourceRefs: []views.SourceRef{{
			Kind: views.SourceDocument,
			Document: &views.DocumentSourceRef{
				DatasetID:   "dataset-1",
				DocumentID:  "doc-1",
				Version:     "1",
				ContentHash: "sha256:abc",
			},
		}},
		Signature: views.ViewSignature{
			ViewID:             views.ID("projected-docs"),
			TransformSignature: "record-transform:v1",
		},
	}
}

func lineageRecord() Record {
	observedAt := time.Date(2026, 6, 9, 8, 30, 15, 123456789, time.UTC)
	sourceRefs := []views.SourceRef{
		{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: "conv-1",
				MessageID:      "msg-1",
				Span:           &views.Span{Start: 2, End: 7},
			},
		},
		{
			Kind: views.SourceDocument,
			Document: &views.DocumentSourceRef{
				DatasetID:   "dataset-1",
				DocumentID:  "doc-1",
				Version:     "3",
				ContentHash: "sha256:abc",
				Span:        &views.Span{Start: 10, End: 42},
			},
		},
	}
	messageKey, err := sourceRefs[0].StableKeyE()
	if err != nil {
		panic(err)
	}
	documentKey, err := sourceRefs[1].StableKeyE()
	if err != nil {
		panic(err)
	}
	return Record{
		ID:         "lineage-record",
		Text:       "hello with lineage",
		SourceRefs: sourceRefs,
		Signature: views.ViewSignature{
			ViewID: views.ID("projected-docs"),
			SourceRevisions: []views.SourceRevision{
				{
					Kind:       views.SourceMessage,
					SourceKey:  messageKey,
					Revision:   "2",
					ObservedAt: observedAt,
				},
				{
					Kind:        views.SourceDocument,
					SourceKey:   documentKey,
					ContentHash: "sha256:def",
					ObservedAt:  observedAt.Add(time.Minute),
				},
			},
			UpstreamViewRefs: []views.UpstreamViewRef{
				{
					ViewID:          views.ID("recent-window"),
					OutputSignature: "window:v1",
					RecordKey:       "window-1",
				},
			},
			TransformSignature: "record-transform:v1",
			DiagnosticSignatures: map[string]string{
				"chunker":  "chunker:v1",
				"embedder": "embedder:v1",
			},
		},
	}
}

func assertDecodedLineage(t *testing.T, metadata map[string]any, wantRefs []views.SourceRef, wantSignature views.ViewSignature) {
	t.Helper()

	refs, ok, err := DecodeSourceRefs(metadata)
	if err != nil {
		t.Fatalf("DecodeSourceRefs() error = %v", err)
	}
	if !ok {
		t.Fatal("DecodeSourceRefs() ok = false, want true")
	}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("DecodeSourceRefs() = %+v, want %+v", refs, wantRefs)
	}

	signature, ok, err := DecodeSignature(metadata)
	if err != nil {
		t.Fatalf("DecodeSignature() error = %v", err)
	}
	if !ok {
		t.Fatal("DecodeSignature() ok = false, want true")
	}
	assertSignatureEqual(t, signature, wantSignature)
}

func assertSignatureEqual(t *testing.T, got, want views.ViewSignature) {
	t.Helper()

	if got.ViewID != want.ViewID {
		t.Fatalf("signature ViewID = %q, want %q", got.ViewID, want.ViewID)
	}
	if got.TransformSignature != want.TransformSignature {
		t.Fatalf("signature TransformSignature = %q, want %q", got.TransformSignature, want.TransformSignature)
	}
	if !reflect.DeepEqual(got.UpstreamViewRefs, want.UpstreamViewRefs) {
		t.Fatalf("signature UpstreamViewRefs = %+v, want %+v", got.UpstreamViewRefs, want.UpstreamViewRefs)
	}
	if !reflect.DeepEqual(got.DiagnosticSignatures, want.DiagnosticSignatures) {
		t.Fatalf("signature DiagnosticSignatures = %+v, want %+v", got.DiagnosticSignatures, want.DiagnosticSignatures)
	}
	if len(got.SourceRevisions) != len(want.SourceRevisions) {
		t.Fatalf("signature SourceRevisions len = %d, want %d", len(got.SourceRevisions), len(want.SourceRevisions))
	}
	for i := range got.SourceRevisions {
		gotRev := got.SourceRevisions[i]
		wantRev := want.SourceRevisions[i]
		if gotRev.Kind != wantRev.Kind ||
			gotRev.SourceKey != wantRev.SourceKey ||
			gotRev.Revision != wantRev.Revision ||
			gotRev.ContentHash != wantRev.ContentHash ||
			!gotRev.ObservedAt.Equal(wantRev.ObservedAt) {
			t.Fatalf("signature SourceRevisions[%d] = %+v, want %+v", i, gotRev, wantRev)
		}
	}
}

type fakeIndex struct {
	upsertNamespace string
	upsertDocs      []retrieval.Doc
	deleteNamespace string
	deleteIDs       []string
}

func (f *fakeIndex) Upsert(_ context.Context, namespace string, docs []retrieval.Doc) error {
	f.upsertNamespace = namespace
	f.upsertDocs = make([]retrieval.Doc, len(docs))
	for i, doc := range docs {
		f.upsertDocs[i] = retrieval.CloneDoc(doc)
	}
	return nil
}

func (f *fakeIndex) Delete(_ context.Context, namespace string, ids []string) error {
	f.deleteNamespace = namespace
	f.deleteIDs = append([]string(nil), ids...)
	return nil
}

func (f *fakeIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}

func (f *fakeIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}

func (f *fakeIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{}
}

func (f *fakeIndex) Close() error {
	return nil
}

type droppableFakeIndex struct {
	*fakeIndex
	dropNamespace string
}

func (f *droppableFakeIndex) Drop(_ context.Context, namespace string) error {
	f.dropNamespace = namespace
	return nil
}

type fakeEmbedder struct {
	batchTexts   []string
	batchVectors [][]float32
}

func (f *fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.batchTexts = append(f.batchTexts, texts...)
	vectors := make([][]float32, len(f.batchVectors))
	for i, vector := range f.batchVectors {
		vectors[i] = append([]float32(nil), vector...)
	}
	return vectors, nil
}

type blockingEmbedder struct{}

func (blockingEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingEmbedder) EmbedBatch(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
