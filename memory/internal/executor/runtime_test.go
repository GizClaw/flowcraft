package executor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalworkspace "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestExecutorPacksRecentMessages(t *testing.T) {
	ctx := context.Background()
	store := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "sources/message"))
	_, err := store.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv",
		Messages: []sourcemessage.Message{{
			Message: model.NewTextMessage(model.RoleUser, "hello"),
		}},
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	rt, err := New(Deps{
		Assembly: compiler.Assembly{
			Sources: []compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
			Views: []compiler.ViewAssembly{{
				Capability: compiler.CapabilityRecentWindow,
				Descriptor: views.Descriptor{
					ID:      recent.DefaultWindowID,
					Kind:    views.KindRecentWindow,
					Version: recent.DefaultWindowVersion,
				},
				Required: true,
			}},
		},
		MessageStore: store,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	pack, err := rt.PackContext(ctx, PackContextRequest{
		Window: recent.WindowRequest{Scope: views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.Items) != 1 || pack.Items[0].Text != "user: hello" {
		t.Fatalf("Items = %+v, want recent message", pack.Items)
	}
}

func TestExecutorProvidesSourceMessageResolverToContextPacker(t *testing.T) {
	ctx := context.Background()
	store := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "sources/message"))
	if _, err := store.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv",
		Messages: []sourcemessage.Message{{
			ID:      "m1",
			Message: model.NewTextMessage(model.RoleUser, "source evidence"),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	packer := &resolverRecordingPacker{}
	rt, err := New(Deps{
		Assembly: compiler.Assembly{
			Sources: []compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
			Views: []compiler.ViewAssembly{{
				Capability: compiler.CapabilityRecentWindow,
				Descriptor: views.Descriptor{
					ID:      recent.DefaultWindowID,
					Kind:    views.KindRecentWindow,
					Version: recent.DefaultWindowVersion,
				},
				Required: true,
			}},
		},
		MessageStore:  store,
		ContextPacker: packer,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := rt.PackContext(ctx, PackContextRequest{
		Window: recent.WindowRequest{Scope: views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}},
	}); err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if !packer.resolved {
		t.Fatal("ContextPacker did not receive a working source message resolver")
	}
}

type resolverRecordingPacker struct {
	resolved bool
}

func (p *resolverRecordingPacker) PackContext(ctx context.Context, input derive.ContextPackInput) (derive.ContextPackOutput, error) {
	if input.SourceMessages != nil {
		msg, ok, err := input.SourceMessages.GetSourceMessage(ctx, "conv", "m1")
		if err != nil {
			return derive.ContextPackOutput{}, err
		}
		p.resolved = ok && msg.ID == "m1"
	}
	return derive.ContextPackOutput{Items: input.Items}, nil
}

func TestIndexMessagesWritesChunkRecordsAndSearchHydratesUniqueMessages(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	store := sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message"))
	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      []compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		Capabilities: []compiler.CapabilitySpec{{Capability: compiler.CapabilityMessageIndex, Required: true}},
		Projections:  []compiler.ProjectionRequest{{Capability: compiler.CapabilityMessageIndex, Namespace: "message_index", Required: true}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: store,
		Index:        index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := views.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv", DatasetID: "dataset"}
	if _, err := store.Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{{
			ID:      "dia-long",
			Message: model.NewTextMessage(model.RoleUser, strings.Repeat("deepneedle alpha beta. ", 900)),
		}},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	messages, err := rt.IndexMessages(ctx, recent.WindowRequest{
		Scope:  scope,
		Budget: &recent.WindowBudget{MaxMessages: 10},
	}, "")
	if err != nil {
		t.Fatalf("IndexMessages() error = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("IndexMessages() messages = %d, want 1", len(messages))
	}
	namespace := "message_index"
	listResp, err := index.List(ctx, namespace, retrieval.ListRequest{PageSize: 100})
	if err != nil {
		t.Fatalf("List(indexed chunks) error = %v", err)
	}
	if len(listResp.Items) < 2 {
		t.Fatalf("indexed docs = %d, want multiple chunk docs", len(listResp.Items))
	}
	for _, doc := range listResp.Items {
		if got, want := doc.Metadata[projectors.MetadataMessageIDKey], "dia-long"; got != want {
			t.Fatalf("doc %q metadata message id = %v, want %q", doc.ID, got, want)
		}
		if _, ok := doc.Metadata[projectors.MetadataMessageChunkIndex]; !ok {
			t.Fatalf("doc %q missing chunk index metadata", doc.ID)
		}
	}

	searchResp, err := rt.SearchSourceMessages(ctx, retrieval.SearchRequest{QueryText: "deepneedle", TopK: 10, MinScore: 0.0001}, "")
	if err != nil {
		t.Fatalf("SearchSourceMessages() error = %v", err)
	}
	if len(searchResp.Hits) != 1 {
		t.Fatalf("SearchSourceMessages() hits = %d, want one hydrated message after chunk dedupe", len(searchResp.Hits))
	}
	if got, want := searchResp.Hits[0].Message.ID, "dia-long"; got != want {
		t.Fatalf("SearchSourceMessages() message id = %q, want %q", got, want)
	}
	if got := searchResp.Hits[0].Retrieval.Doc.Metadata[projectors.MetadataMessageChunkIndex]; got == nil {
		t.Fatalf("representative hit missing chunk metadata")
	}
}

func TestIndexDocumentRequiresChunkerAtExecution(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      []compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		Capabilities: []compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	rt, err := New(Deps{
		Assembly:      assembly,
		DocumentStore: sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document")),
		ChunkStore:    viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks")),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = rt.IndexDocument(ctx, views.Scope{RuntimeID: "rt", UserID: "user", DatasetID: "dataset"}, "doc-1", "")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("IndexDocument() error = %v, want NotAvailable without DocumentChunker", err)
	}
}

func TestBuildSummaryDAGRequiresSummarizerAtExecution(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	assembly, err := compiler.Compile(compiler.Spec{
		Sources: []compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		Capabilities: []compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilitySummaryDAG, Required: true},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/message")),
		SummaryStore: recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(root, "views/summary_dag")),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = rt.BuildSummaryDAG(ctx, recent.WindowRequest{
		Scope: views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"},
	}, "")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("BuildSummaryDAG() error = %v, want NotAvailable without Summarizer", err)
	}
}

func TestIndexDocumentRebuildDeletesOldChunksAndProjectionRecords(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"), retrievalworkspace.WithAutoCompact(false))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	defer func() { _ = index.Close() }()

	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      []compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		Capabilities: []compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
		Projections:  []compiler.ProjectionRequest{{Capability: compiler.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	chunkStore := viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks"))
	chunker := &scriptedDocumentChunker{
		batches: [][]string{
			{"alpha old chunk", "retiredonly bravo", "retiredonly charlie"},
			{"freshonly chunk"},
		},
	}
	rt, err := New(Deps{
		Assembly:        assembly,
		DocumentStore:   documentStore,
		ChunkStore:      chunkStore,
		Index:           index,
		DocumentChunker: chunker,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := views.Scope{RuntimeID: "rt", UserID: "user", DatasetID: "dataset"}
	namespace := "document_chunks_override"
	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "first document revision",
	}}); err != nil {
		t.Fatalf("Put(first document) error = %v", err)
	}
	firstChunks, err := rt.IndexDocument(ctx, scope, "doc-1", namespace)
	if err != nil {
		t.Fatalf("IndexDocument(first) error = %v", err)
	}
	if len(firstChunks) != 3 {
		t.Fatalf("first IndexDocument chunks = %d, want 3", len(firstChunks))
	}
	oldRecordID := projectors.DocumentChunkRecordID(scope.DatasetID, "doc-1", "chunk-1")
	assertProjectionContains(t, ctx, index, namespace, oldRecordID)

	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "second document revision",
	}}); err != nil {
		t.Fatalf("Put(second document) error = %v", err)
	}
	secondChunks, err := rt.IndexDocument(ctx, scope, "doc-1", namespace)
	if err != nil {
		t.Fatalf("IndexDocument(second) error = %v", err)
	}
	if len(secondChunks) != 1 {
		t.Fatalf("second IndexDocument chunks = %d, want 1", len(secondChunks))
	}

	listedChunks, err := chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &scope})
	if err != nil {
		t.Fatalf("ListChunks(after rebuild) error = %v", err)
	}
	if len(listedChunks) != 1 || listedChunks[0].ID != "chunk-0" || listedChunks[0].Text != "freshonly chunk" {
		t.Fatalf("ListChunks(after rebuild) = %+v, want only fresh chunk-0", listedChunks)
	}
	if got, ok, err := chunkStore.GetChunk(ctx, scope, "doc-1", "chunk-1"); err != nil || ok {
		t.Fatalf("GetChunk(old chunk) got=%+v ok=%v err=%v, want miss", got, ok, err)
	}

	getter, ok := retrieval.AsDocGetter(index)
	if !ok {
		t.Fatal("workspace index should expose DocGetter")
	}
	if got, ok, err := getter.Get(ctx, namespace, oldRecordID); err != nil || ok {
		t.Fatalf("projection Get(old record) got=%+v ok=%v err=%v, want miss", got, ok, err)
	}
	assertProjectionMissing(t, ctx, index, namespace, oldRecordID)
	searchOld, err := index.Search(ctx, namespace, retrieval.SearchRequest{QueryText: "retiredonly", TopK: 10, MinScore: 0.0001})
	if err != nil {
		t.Fatalf("Search(old token) error = %v", err)
	}
	if len(searchOld.Hits) != 0 {
		t.Fatalf("Search(old token) hits = %+v, want none", searchOld.Hits)
	}
	searchFresh, err := index.Search(ctx, namespace, retrieval.SearchRequest{QueryText: "freshonly", TopK: 10, MinScore: 0.0001})
	if err != nil {
		t.Fatalf("Search(fresh token) error = %v", err)
	}
	if len(searchFresh.Hits) == 0 || searchFresh.Hits[0].Doc.Content != "freshonly chunk" {
		t.Fatalf("Search(fresh token) hits = %+v, want fresh chunk", searchFresh.Hits)
	}
}

func TestIndexDocumentRebuildWithoutProjectionDeletesStaleChunks(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      []compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		Capabilities: []compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	chunkStore := viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks"))
	rt, err := New(Deps{
		Assembly:        assembly,
		DocumentStore:   documentStore,
		ChunkStore:      chunkStore,
		DocumentChunker: &scriptedDocumentChunker{batches: [][]string{{"alpha old chunk", "retiredonly bravo"}, {"freshonly chunk"}}},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := views.Scope{RuntimeID: "rt", UserID: "user", DatasetID: "dataset"}
	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "first document revision",
	}}); err != nil {
		t.Fatalf("Put(first document) error = %v", err)
	}
	if _, err := rt.IndexDocument(ctx, scope, "doc-1", ""); err != nil {
		t.Fatalf("IndexDocument(first) error = %v", err)
	}
	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "second document revision",
	}}); err != nil {
		t.Fatalf("Put(second document) error = %v", err)
	}
	if _, err := rt.IndexDocument(ctx, scope, "doc-1", ""); err != nil {
		t.Fatalf("IndexDocument(second) error = %v", err)
	}

	listedChunks, err := chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &scope})
	if err != nil {
		t.Fatalf("ListChunks(after rebuild) error = %v", err)
	}
	if len(listedChunks) != 1 || listedChunks[0].ID != "chunk-0" || listedChunks[0].Text != "freshonly chunk" {
		t.Fatalf("ListChunks(after rebuild) = %+v, want only fresh chunk-0", listedChunks)
	}
	if got, ok, err := chunkStore.GetChunk(ctx, scope, "doc-1", "chunk-1"); err != nil || ok {
		t.Fatalf("GetChunk(old chunk) got=%+v ok=%v err=%v, want miss", got, ok, err)
	}
}

func TestIndexDocumentChunkerFailureKeepsOldChunksAndProjectionRecords(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(root, "retrieval"), retrievalworkspace.WithAutoCompact(false))
	if err != nil {
		t.Fatalf("retrieval workspace: %v", err)
	}
	defer func() { _ = index.Close() }()

	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      []compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		Capabilities: []compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
		Projections:  []compiler.ProjectionRequest{{Capability: compiler.CapabilityDocumentChunks, Namespace: "document_chunks", Required: true}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	documentStore := sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(root, "sources/document"))
	chunkStore := viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(root, "views/document_chunks"))
	chunkerErr := errors.New("forced chunker failure")
	rt, err := New(Deps{
		Assembly:        assembly,
		DocumentStore:   documentStore,
		ChunkStore:      chunkStore,
		Index:           index,
		DocumentChunker: &scriptedDocumentChunker{batches: [][]string{{"alpha old chunk", "retiredonly bravo"}}, errs: []error{nil, chunkerErr}},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	scope := views.Scope{RuntimeID: "rt", UserID: "user", DatasetID: "dataset"}
	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "first document revision",
	}}); err != nil {
		t.Fatalf("Put(first document) error = %v", err)
	}
	if _, err := rt.IndexDocument(ctx, scope, "doc-1", ""); err != nil {
		t.Fatalf("IndexDocument(first) error = %v", err)
	}
	oldRecordID := projectors.DocumentChunkRecordID(scope.DatasetID, "doc-1", "chunk-1")
	assertProjectionContains(t, ctx, index, "document_chunks", oldRecordID)

	if _, err := documentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: scope.DatasetID,
		ID:        "doc-1",
		Content:   "second document revision",
	}}); err != nil {
		t.Fatalf("Put(second document) error = %v", err)
	}
	if _, err := rt.IndexDocument(ctx, scope, "doc-1", ""); !errors.Is(err, chunkerErr) {
		t.Fatalf("IndexDocument(second) error = %v, want %v", err, chunkerErr)
	}

	listedChunks, err := chunkStore.ListChunks(ctx, "doc-1", viewdocument.ListOptions{Scope: &scope})
	if err != nil {
		t.Fatalf("ListChunks(after failed rebuild) error = %v", err)
	}
	if len(listedChunks) != 2 || listedChunks[0].Text != "alpha old chunk" || listedChunks[1].Text != "retiredonly bravo" {
		t.Fatalf("ListChunks(after failed rebuild) = %+v, want original chunks", listedChunks)
	}
	assertProjectionContains(t, ctx, index, "document_chunks", oldRecordID)
	searchOld, err := index.Search(ctx, "document_chunks", retrieval.SearchRequest{QueryText: "retiredonly", TopK: 10, MinScore: 0.0001})
	if err != nil {
		t.Fatalf("Search(old token) error = %v", err)
	}
	if len(searchOld.Hits) == 0 || searchOld.Hits[0].Doc.ID != oldRecordID {
		t.Fatalf("Search(old token) hits = %+v, want old projection", searchOld.Hits)
	}
}

type scriptedDocumentChunker struct {
	batches [][]string
	errs    []error
	calls   int
}

func (c *scriptedDocumentChunker) ChunkDocument(_ context.Context, input derive.DocumentChunkInput) ([]viewdocument.Chunk, error) {
	call := c.calls
	c.calls++
	if call < len(c.errs) && c.errs[call] != nil {
		return nil, c.errs[call]
	}
	if call >= len(c.batches) {
		return nil, nil
	}
	texts := c.batches[call]

	chunks := make([]viewdocument.Chunk, 0, len(texts))
	for i, text := range texts {
		span := views.Span{Start: i * 100, End: i*100 + len(text)}
		ref := views.SourceRef{
			Kind: views.SourceDocument,
			Document: &views.DocumentSourceRef{
				DatasetID:   input.Document.DatasetID,
				DocumentID:  input.Document.ID,
				Version:     strconv.FormatUint(input.Document.Version, 10),
				ContentHash: input.Document.ContentHash,
				Span:        &span,
			},
		}
		chunks = append(chunks, viewdocument.Chunk{
			ID:         viewdocument.ChunkID("chunk-" + strconv.Itoa(i)),
			Scope:      input.Scope,
			DocumentID: input.Document.ID,
			Layer: viewdocument.Layer{
				Name:               "scripted",
				Version:            "v1",
				TransformSignature: "scripted:v1",
			},
			Ordinal:   i,
			Span:      span,
			Text:      text,
			SourceRef: ref,
			Signature: views.ViewSignature{
				ViewID: input.View.ID,
				SourceRevisions: []views.SourceRevision{{
					Kind:        views.SourceDocument,
					SourceKey:   ref.StableKey(),
					Revision:    strconv.FormatUint(input.Document.Version, 10),
					ContentHash: input.Document.ContentHash,
					ObservedAt:  input.Document.UpdatedAt,
				}},
				TransformSignature: "scripted:v1",
			},
		})
	}
	return chunks, nil
}

func assertProjectionContains(t *testing.T, ctx context.Context, index retrieval.Index, namespace, id string) {
	t.Helper()
	resp, err := index.List(ctx, namespace, retrieval.ListRequest{PageSize: 10, OrderBy: retrieval.OrderByIDAsc})
	if err != nil {
		t.Fatalf("List(%s) error = %v", namespace, err)
	}
	for _, doc := range resp.Items {
		if doc.ID == id {
			return
		}
	}
	t.Fatalf("List(%s) ids = %v, want %q present", namespace, projectionIDs(resp.Items), id)
}

func assertProjectionMissing(t *testing.T, ctx context.Context, index retrieval.Index, namespace, id string) {
	t.Helper()
	resp, err := index.List(ctx, namespace, retrieval.ListRequest{PageSize: 10, OrderBy: retrieval.OrderByIDAsc})
	if err != nil {
		t.Fatalf("List(%s) error = %v", namespace, err)
	}
	for _, doc := range resp.Items {
		if doc.ID == id {
			t.Fatalf("List(%s) ids = %v, want %q deleted", namespace, projectionIDs(resp.Items), id)
		}
	}
}

func projectionIDs(docs []retrieval.Doc) []string {
	ids := make([]string, 0, len(docs))
	for _, doc := range docs {
		ids = append(ids, doc.ID)
	}
	return ids
}
