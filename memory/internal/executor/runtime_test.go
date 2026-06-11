package executor

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	retrievalworkspace "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestNewRequiredSummaryDAGMissingSummarizerFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilitySummaryDAG, Required: true}},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilitySummaryDAG, Namespace: "summary_nodes", Required: true}},
	)

	_, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
		SummaryStore: newSummaryStore(),
	})
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "Summarizer") {
		t.Fatalf("New() err = %v, want validation error mentioning Summarizer", err)
	}
}

func TestNewRequiredObservationLedgerMissingExtractorFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityObservationLedger, Required: true}},
		nil,
	)

	_, err := New(Deps{
		Assembly:         assembly,
		MessageStore:     newMessageStore(),
		ObservationStore: newObservationStore(),
	})
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "ObservationExtractor") {
		t.Fatalf("New() err = %v, want validation error mentioning ObservationExtractor", err)
	}
}

func TestNewRequiredDocumentChunksMissingChunkerFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
		nil,
	)

	_, err := New(Deps{
		Assembly:      assembly,
		DocumentStore: newDocumentStore(),
		ChunkStore:    newChunkStore(),
	})
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "DocumentChunker") {
		t.Fatalf("New() err = %v, want validation error mentioning DocumentChunker", err)
	}
}

func TestNewRequiredFactLedgerMissingReconcilerFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
		},
		nil,
	)
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}

	_, err := New(deps)
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "FactReconciler") {
		t.Fatalf("New() err = %v, want validation error mentioning FactReconciler", err)
	}
}

func TestNewRequiredFactLedgerMissingStoreFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
		},
		nil,
	)
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactStore = nil

	_, err := New(deps)
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "FactStore") {
		t.Fatalf("New() err = %v, want validation error mentioning FactStore", err)
	}
}

func TestNewRequiredFactGraphMissingBuilderFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
		},
		nil,
	)
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}

	_, err := New(deps)
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "FactGraphBuilder") {
		t.Fatalf("New() err = %v, want validation error mentioning FactGraphBuilder", err)
	}
}

func TestNewRequiredFactGraphMissingStoreFails(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
		},
		nil,
	)
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.FactGraphStore = nil

	_, err := New(deps)
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "FactGraphStore") {
		t.Fatalf("New() err = %v, want validation error mentioning FactGraphStore", err)
	}
}

func TestNewRequiredSourcesMissingStoresFailValidation(t *testing.T) {
	tests := []struct {
		name string
		spec compiler.SourceSpec
		want string
	}{
		{
			name: "message log",
			spec: compiler.SourceSpec{Kind: compiler.SourceMessageLog, Required: true},
			want: "MessageStore",
		},
		{
			name: "document store",
			spec: compiler.SourceSpec{Kind: compiler.SourceDocumentStore, Required: true},
			want: "DocumentStore",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembly := compileAssembly(t, []compiler.SourceSpec{tt.spec}, nil, nil)

			_, err := New(Deps{Assembly: assembly})
			if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() err = %v, want validation error mentioning %s", err, tt.want)
			}
		})
	}
}

func TestOptionalSummaryDAGWithoutSummarizerSkipsFlow(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilitySummaryDAG}},
		nil,
	)

	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
	})
	if err != nil {
		t.Fatalf("New(optional summary without summarizer) error = %v", err)
	}

	_, err = rt.BuildSummaryDAG(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, "")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("BuildSummaryDAG() err = %v, want NotAvailable", err)
	}
}

func TestOptionalUnsupportedProjectionDoesNotNeedIndex(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger},
			{Capability: compiler.CapabilityFactLedger},
		},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilityFactLedger, Namespace: "facts"}},
	)

	rt, err := New(Deps{Assembly: assembly})
	if err != nil {
		t.Fatalf("New(optional unsupported projection) error = %v", err)
	}
	if rt.RetrievalIndex() != nil {
		t.Fatal("RetrievalIndex() != nil, want no index for skipped unsupported projection")
	}
	if len(rt.writers) != 0 {
		t.Fatalf("writers len = %d, want 0", len(rt.writers))
	}
}

func TestOptionalSummaryDAGProjectionWithoutSummarizerSkipsWriterAndIndex(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilitySummaryDAG}},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilitySummaryDAG, Namespace: "summary_nodes"}},
	)

	rt, err := New(Deps{Assembly: assembly})
	if err != nil {
		t.Fatalf("New(optional summary projection without summarizer) error = %v", err)
	}
	if rt.RetrievalIndex() != nil {
		t.Fatal("RetrievalIndex() != nil, want no index when summary flow is not configured")
	}
	if rt.writers[compiler.CapabilitySummaryDAG] != nil {
		t.Fatal("summary writer configured, want nil")
	}

	_, err = rt.SearchSummaryNodes(ctx, retrieval.SearchRequest{QueryText: "summary", TopK: 1}, "")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("SearchSummaryNodes() err = %v, want NotAvailable", err)
	}
}

func TestRuntimeIndexesProjectsSearchesAndHydratesViews(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{
			{Kind: compiler.SourceMessageLog, Required: true},
			{Kind: compiler.SourceDocumentStore, Required: true},
		},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilitySummaryDAG, Required: true},
			{Capability: compiler.CapabilityDocumentChunks, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
		},
		[]compiler.ProjectionRequest{
			{Capability: compiler.CapabilityDocumentChunks, Namespace: "doc_chunks", Required: true},
			{Capability: compiler.CapabilitySummaryDAG, Namespace: "summary_nodes", Required: true},
			{Capability: compiler.CapabilityObservationLedger, Namespace: "observations", Required: true},
		},
	)

	chunker := &fakeChunker{}
	summarizer := &fakeSummarizer{}
	extractor := &fakeObservationExtractor{}
	deps := newExecutorDeps(t, assembly)
	deps.DocumentChunker = chunker
	deps.Summarizer = summarizer
	deps.ObservationExtractor = extractor
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(full runtime) error = %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	if rt.RetrievalIndex() == nil {
		t.Fatal("RetrievalIndex() = nil, want shared index")
	}
	assertNamespace(t, rt, compiler.CapabilityDocumentChunks, "doc_chunks")
	assertNamespace(t, rt, compiler.CapabilitySummaryDAG, "summary_nodes")
	assertNamespace(t, rt, compiler.CapabilityObservationLedger, "observations")

	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages: []sourcemessage.Message{
			messageWithText("Ada likes tea."),
			messageWithText("The project summary should mention memory runtime."),
		},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}
	if _, err := rt.DocumentStore().Put(ctx, sourcedocument.PutRequest{
		Document: sourcedocument.Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Content:   "chunkable document evidence about runtime memory",
		},
	}); err != nil {
		t.Fatalf("Put document error = %v", err)
	}

	chunks, err := rt.IndexDocument(ctx, testDocumentScope("dataset-1"), "doc-1", "")
	if err != nil {
		t.Fatalf("IndexDocument() error = %v", err)
	}
	if len(chunks) != 1 || chunks[0].ID != "whole" {
		t.Fatalf("IndexDocument() chunks = %+v, want whole-document chunk", chunks)
	}
	if chunker.calls != 1 {
		t.Fatalf("chunker calls = %d, want 1", chunker.calls)
	}
	chunkResults, err := rt.SearchDocumentChunks(ctx, retrieval.SearchRequest{QueryText: "chunkable", TopK: 5}, "")
	if err != nil {
		t.Fatalf("SearchDocumentChunks() error = %v", err)
	}
	if len(chunkResults.Hits) != 1 || chunkResults.Hits[0].Chunk.ID != chunks[0].ID {
		t.Fatalf("SearchDocumentChunks() hits = %+v, want hydrated chunk", chunkResults.Hits)
	}

	nodes, err := rt.BuildSummaryDAG(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, "")
	if err != nil {
		t.Fatalf("BuildSummaryDAG() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "summary-1" {
		t.Fatalf("BuildSummaryDAG() nodes = %+v, want fake summary", nodes)
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summarizer.calls)
	}
	nodeResults, err := rt.SearchSummaryNodes(ctx, retrieval.SearchRequest{QueryText: "runtime summary", TopK: 5}, "")
	if err != nil {
		t.Fatalf("SearchSummaryNodes() error = %v", err)
	}
	if len(nodeResults.Hits) != 1 || nodeResults.Hits[0].Node.ID != nodes[0].ID {
		t.Fatalf("SearchSummaryNodes() hits = %+v, want hydrated summary node", nodeResults.Hits)
	}

	scope := testScope("conv-1")
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, scope, "")
	if err != nil {
		t.Fatalf("ExtractObservations() error = %v", err)
	}
	if len(observations) != 1 || observations[0].ID != "obs-1" {
		t.Fatalf("ExtractObservations() observations = %+v, want fake observation", observations)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls = %d, want 1", extractor.calls)
	}
	observationResults, err := rt.SearchObservations(ctx, retrieval.SearchRequest{QueryText: "likes tea", TopK: 5})
	if err != nil {
		t.Fatalf("SearchObservations() error = %v", err)
	}
	if len(observationResults.Hits) != 1 || observationResults.Hits[0].Observation.ID != observations[0].ID {
		t.Fatalf("SearchObservations() hits = %+v, want hydrated observation", observationResults.Hits)
	}
}

func TestRuntimeFactLedgerAndGraphVerticalSlice(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
		},
		[]compiler.ProjectionRequest{
			{Capability: compiler.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: compiler.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: compiler.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
		},
	)

	extractor := &fakeObservationExtractor{}
	reconciler := &fakeFactReconciler{}
	graphBuilder := &fakeFactGraphBuilder{}
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = extractor
	deps.FactReconciler = reconciler
	deps.FactGraphBuilder = graphBuilder
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(fact vertical slice runtime) error = %v", err)
	}
	assertNamespace(t, rt, compiler.CapabilityObservationLedger, "observations")
	assertNamespace(t, rt, compiler.CapabilityFactLedger, "facts")
	assertNamespace(t, rt, compiler.CapabilityFactGraph, "fact_graph")

	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages: []sourcemessage.Message{
			messageWithText("Ada likes tea."),
		},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}

	scope := testScope("conv-1")
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, scope, "")
	if err != nil {
		t.Fatalf("ExtractObservations() error = %v", err)
	}
	if len(observations) != 1 || observations[0].ID != "obs-1" {
		t.Fatalf("ExtractObservations() observations = %+v, want fake observation", observations)
	}

	facts, err := rt.ReconcileFacts(ctx, observations)
	if err != nil {
		t.Fatalf("ReconcileFacts() error = %v", err)
	}
	if len(facts) != 1 || facts[0].ID != "fact-1" {
		t.Fatalf("ReconcileFacts() facts = %+v, want fake fact", facts)
	}
	if reconciler.calls != 1 {
		t.Fatalf("reconciler calls = %d, want 1", reconciler.calls)
	}
	factResults, err := rt.SearchFacts(ctx, retrieval.SearchRequest{QueryText: "Ada likes tea", TopK: 5})
	if err != nil {
		t.Fatalf("SearchFacts() error = %v", err)
	}
	if len(factResults.Hits) != 1 || factResults.Hits[0].Fact.ID != facts[0].ID {
		t.Fatalf("SearchFacts() hits = %+v, want hydrated fact", factResults.Hits)
	}

	graph, err := rt.BuildFactGraph(ctx, facts)
	if err != nil {
		t.Fatalf("BuildFactGraph() error = %v", err)
	}
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("BuildFactGraph() result = %+v, want two nodes and one edge", graph)
	}
	if graphBuilder.calls != 1 {
		t.Fatalf("graph builder calls = %d, want 1", graphBuilder.calls)
	}
	graphResults, err := rt.SearchFactGraph(ctx, retrieval.SearchRequest{QueryText: "Ada tea likes", TopK: 10})
	if err != nil {
		t.Fatalf("SearchFactGraph() error = %v", err)
	}
	var nodeHits, edgeHits int
	for _, hit := range graphResults.Hits {
		if hit.Node != nil {
			nodeHits++
		}
		if hit.Edge != nil {
			edgeHits++
		}
		if hit.Node == nil && hit.Edge == nil {
			t.Fatalf("SearchFactGraph() hit missing node and edge: %+v", hit)
		}
	}
	if nodeHits == 0 || edgeHits == 0 {
		t.Fatalf("SearchFactGraph() hits = %+v, want at least one hydrated node and edge", graphResults.Hits)
	}
}

func TestPackContextIncludesRecentSummaryDocumentObservationFactAndGraphItems(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{
			{Kind: compiler.SourceMessageLog, Required: true},
			{Kind: compiler.SourceDocumentStore, Required: true},
		},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilitySummaryDAG, Required: true},
			{Capability: compiler.CapabilityDocumentChunks, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
		},
		[]compiler.ProjectionRequest{
			{Capability: compiler.CapabilitySummaryDAG, Namespace: "summary_nodes", Required: true},
			{Capability: compiler.CapabilityDocumentChunks, Namespace: "doc_chunks", Required: true},
			{Capability: compiler.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: compiler.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: compiler.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
		},
	)
	deps := newExecutorDeps(t, assembly)
	deps.DocumentChunker = &fakeChunker{}
	deps.Summarizer = &fakeSummarizer{}
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(full pack runtime) error = %v", err)
	}

	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages: []sourcemessage.Message{
			messageWithText("Ada likes tea."),
			messageWithText("The project summary should mention memory runtime."),
		},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}
	if _, err := rt.DocumentStore().Put(ctx, sourcedocument.PutRequest{
		Document: sourcedocument.Document{
			DatasetID: "dataset-1",
			ID:        "doc-1",
			Content:   "chunkable document evidence about runtime memory",
		},
	}); err != nil {
		t.Fatalf("Put document error = %v", err)
	}
	chunks, err := rt.IndexDocument(ctx, testDocumentScope("dataset-1"), "doc-1", "")
	if err != nil {
		t.Fatalf("IndexDocument() error = %v", err)
	}
	nodes, err := rt.BuildSummaryDAG(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, "")
	if err != nil {
		t.Fatalf("BuildSummaryDAG() error = %v", err)
	}
	scope := testScope("conv-1")
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: testScope("conv-1")}, scope, "")
	if err != nil {
		t.Fatalf("ExtractObservations() error = %v", err)
	}
	facts, err := rt.ReconcileFacts(ctx, observations)
	if err != nil {
		t.Fatalf("ReconcileFacts() error = %v", err)
	}
	graph, err := rt.BuildFactGraph(ctx, facts)
	if err != nil {
		t.Fatalf("BuildFactGraph() error = %v", err)
	}

	summarySearch := retrieval.SearchRequest{QueryText: "runtime summary", TopK: 5}
	documentSearch := retrieval.SearchRequest{QueryText: "chunkable", TopK: 5}
	observationSearch := retrieval.SearchRequest{QueryText: "likes tea", TopK: 5}
	factSearch := retrieval.SearchRequest{QueryText: "Ada likes tea", TopK: 5}
	factGraphSearch := retrieval.SearchRequest{QueryText: "Ada tea likes", TopK: 10}
	pack, err := rt.PackContext(ctx, PackContextRequest{
		Window:            recent.WindowRequest{Scope: testScope("conv-1")},
		SummarySearch:     &summarySearch,
		DocumentSearch:    &documentSearch,
		ObservationSearch: &observationSearch,
		FactSearch:        &factSearch,
		FactGraphSearch:   &factGraphSearch,
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}

	if len(pack.Window.Messages) != 2 {
		t.Fatalf("Window.Messages len = %d, want 2", len(pack.Window.Messages))
	}
	if len(pack.SummaryHits) != 1 || pack.SummaryHits[0].Node.ID != nodes[0].ID {
		t.Fatalf("SummaryHits = %+v, want hydrated summary node", pack.SummaryHits)
	}
	if len(pack.DocumentHits) != 1 || pack.DocumentHits[0].Chunk.ID != chunks[0].ID {
		t.Fatalf("DocumentHits = %+v, want hydrated document chunk", pack.DocumentHits)
	}
	if len(pack.ObservationHits) != 1 || pack.ObservationHits[0].Observation.ID != observations[0].ID {
		t.Fatalf("ObservationHits = %+v, want hydrated observation", pack.ObservationHits)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.ID != facts[0].ID {
		t.Fatalf("FactHits = %+v, want hydrated fact", pack.FactHits)
	}
	if len(pack.FactGraphHits) != len(graph.Nodes)+len(graph.Edges) {
		t.Fatalf("FactGraphHits len = %d, want %d: %+v", len(pack.FactGraphHits), len(graph.Nodes)+len(graph.Edges), pack.FactGraphHits)
	}
	if len(pack.Items) != 6+len(pack.FactGraphHits) {
		t.Fatalf("Items len = %d, want %d: %+v", len(pack.Items), 6+len(pack.FactGraphHits), pack.Items)
	}

	wantKinds := []ContextItemKind{
		ContextItemRecentMessage,
		ContextItemRecentMessage,
		ContextItemSummaryNode,
		ContextItemDocumentChunk,
		ContextItemObservation,
		ContextItemFact,
	}
	wantTexts := []string{
		"user: Ada likes tea.",
		"user: The project summary should mention memory runtime.",
		"runtime summary about memory",
		"chunkable document evidence about runtime memory",
		"user:ada likes tea",
		"user:ada likes tea",
	}
	for i := range wantKinds {
		if pack.Items[i].Kind != wantKinds[i] || pack.Items[i].Text != wantTexts[i] {
			t.Fatalf("Items[%d] = (%q, %q), want (%q, %q)", i, pack.Items[i].Kind, pack.Items[i].Text, wantKinds[i], wantTexts[i])
		}
	}
	if pack.Items[0].Message == nil || pack.Items[1].Message == nil {
		t.Fatalf("recent message items missing message pointers: %+v", pack.Items[:2])
	}
	if pack.Items[2].SummaryNode == nil || pack.Items[2].Retrieval == nil {
		t.Fatalf("summary item missing hydrated node or retrieval: %+v", pack.Items[2])
	}
	if pack.Items[3].DocumentChunk == nil || pack.Items[3].Retrieval == nil {
		t.Fatalf("document item missing hydrated chunk or retrieval: %+v", pack.Items[3])
	}
	if pack.Items[4].Observation == nil || pack.Items[4].Retrieval == nil {
		t.Fatalf("observation item missing hydrated record or retrieval: %+v", pack.Items[4])
	}
	if pack.Items[5].Fact == nil || pack.Items[5].Retrieval == nil {
		t.Fatalf("fact item missing hydrated record or retrieval: %+v", pack.Items[5])
	}
	var graphNodeItems, graphEdgeItems int
	for i, hit := range pack.FactGraphHits {
		item := pack.Items[6+i]
		switch {
		case hit.Node != nil:
			graphNodeItems++
			if item.Kind != ContextItemFactGraphNode || item.FactGraphNode == nil || item.Retrieval == nil || item.Text != hit.Node.Label {
				t.Fatalf("graph node item for hit %d = %+v, want hydrated node text %q", i, item, hit.Node.Label)
			}
		case hit.Edge != nil:
			graphEdgeItems++
			wantText := string(hit.Edge.From) + " " + hit.Edge.Predicate + " " + string(hit.Edge.To)
			if item.Kind != ContextItemFactGraphEdge || item.FactGraphEdge == nil || item.Retrieval == nil || item.Text != wantText {
				t.Fatalf("graph edge item for hit %d = %+v, want hydrated edge text %q", i, item, wantText)
			}
		default:
			t.Fatalf("FactGraphHits[%d] missing node and edge: %+v", i, hit)
		}
	}
	if graphNodeItems == 0 || graphEdgeItems == 0 {
		t.Fatalf("graph items include nodes:%d edges:%d, want at least one of each", graphNodeItems, graphEdgeItems)
	}
}

func TestPackContextWindowOnlyDoesNotNeedRetrieval(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityRecentWindow, Required: true}},
		nil,
	)
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
	})
	if err != nil {
		t.Fatalf("New(window-only runtime) error = %v", err)
	}
	if rt.RetrievalIndex() != nil {
		t.Fatal("RetrievalIndex() != nil, want no index for window-only pack")
	}
	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages:       []sourcemessage.Message{messageWithText("only recent context")},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}

	pack, err := rt.PackContext(ctx, PackContextRequest{
		Window: recent.WindowRequest{Scope: testScope("conv-1")},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.SummaryHits) != 0 || len(pack.DocumentHits) != 0 {
		t.Fatalf("search hits = summary:%d document:%d, want none", len(pack.SummaryHits), len(pack.DocumentHits))
	}
	if len(pack.Items) != 1 {
		t.Fatalf("Items len = %d, want 1: %+v", len(pack.Items), pack.Items)
	}
	if item := pack.Items[0]; item.Kind != ContextItemRecentMessage || item.Text != "user: only recent context" || item.Retrieval != nil {
		t.Fatalf("Items[0] = %+v, want recent message text without retrieval", item)
	}
}

func TestPackContextSummarySearchWithoutProjectionReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilitySummaryDAG, Required: true},
		},
		nil,
	)
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
		SummaryStore: newSummaryStore(),
		Summarizer:   &fakeSummarizer{},
	})
	if err != nil {
		t.Fatalf("New(summary runtime without projection) error = %v", err)
	}

	summarySearch := retrieval.SearchRequest{QueryText: "summary", TopK: 1}
	_, err = rt.PackContext(ctx, PackContextRequest{
		Window:        recent.WindowRequest{Scope: testScope("conv-1")},
		SummarySearch: &summarySearch,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextDocumentSearchWithoutProjectionReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{
			{Kind: compiler.SourceMessageLog, Required: true},
			{Kind: compiler.SourceDocumentStore, Required: true},
		},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityDocumentChunks, Required: true},
		},
		nil,
	)
	rt, err := New(Deps{
		Assembly:        assembly,
		MessageStore:    newMessageStore(),
		DocumentStore:   newDocumentStore(),
		ChunkStore:      newChunkStore(),
		DocumentChunker: &fakeChunker{},
	})
	if err != nil {
		t.Fatalf("New(document runtime without projection) error = %v", err)
	}

	documentSearch := retrieval.SearchRequest{QueryText: "document", TopK: 1}
	_, err = rt.PackContext(ctx, PackContextRequest{
		Window:         recent.WindowRequest{Scope: testScope("conv-1")},
		DocumentSearch: &documentSearch,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextObservationSearchWithoutProjectionReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
		},
		nil,
	)
	rt, err := New(Deps{
		Assembly:             assembly,
		MessageStore:         newMessageStore(),
		ObservationStore:     newObservationStore(),
		ObservationExtractor: &fakeObservationExtractor{},
	})
	if err != nil {
		t.Fatalf("New(observation runtime without projection) error = %v", err)
	}

	observationSearch := retrieval.SearchRequest{QueryText: "observation", TopK: 1}
	_, err = rt.PackContext(ctx, PackContextRequest{
		Window:            recent.WindowRequest{Scope: testScope("conv-1")},
		ObservationSearch: &observationSearch,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextFactSearchWithoutProjectionReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
		},
		nil,
	)
	rt, err := New(Deps{
		Assembly:             assembly,
		MessageStore:         newMessageStore(),
		ObservationStore:     newObservationStore(),
		FactStore:            newFactStore(),
		ObservationExtractor: &fakeObservationExtractor{},
		FactReconciler:       &fakeFactReconciler{},
	})
	if err != nil {
		t.Fatalf("New(fact runtime without projection) error = %v", err)
	}

	factSearch := retrieval.SearchRequest{QueryText: "fact", TopK: 1}
	_, err = rt.PackContext(ctx, PackContextRequest{
		Window:     recent.WindowRequest{Scope: testScope("conv-1")},
		FactSearch: &factSearch,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextFactGraphSearchWithoutProjectionReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
		},
		nil,
	)
	rt, err := New(Deps{
		Assembly:             assembly,
		MessageStore:         newMessageStore(),
		ObservationStore:     newObservationStore(),
		FactStore:            newFactStore(),
		FactGraphStore:       newFactGraphStore(),
		ObservationExtractor: &fakeObservationExtractor{},
		FactReconciler:       &fakeFactReconciler{},
		FactGraphBuilder:     &fakeFactGraphBuilder{},
	})
	if err != nil {
		t.Fatalf("New(fact graph runtime without projection) error = %v", err)
	}

	factGraphSearch := retrieval.SearchRequest{QueryText: "graph", TopK: 1}
	_, err = rt.PackContext(ctx, PackContextRequest{
		Window:          recent.WindowRequest{Scope: testScope("conv-1")},
		FactGraphSearch: &factGraphSearch,
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextWithoutRecentWindowReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
		nil,
	)
	rt, err := New(Deps{
		Assembly:        assembly,
		DocumentStore:   newDocumentStore(),
		ChunkStore:      newChunkStore(),
		DocumentChunker: &fakeChunker{},
	})
	if err != nil {
		t.Fatalf("New(document-only runtime) error = %v", err)
	}

	_, err = rt.PackContext(ctx, PackContextRequest{
		Window: recent.WindowRequest{Scope: testScope("conv-1")},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("PackContext() err = %v, want NotAvailable", err)
	}
}

func TestPackContextSkipsEmptyMessageItemsButKeepsWindow(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityRecentWindow, Required: true}},
		nil,
	)
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
	})
	if err != nil {
		t.Fatalf("New(window runtime) error = %v", err)
	}
	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages: []sourcemessage.Message{{
			Message: model.Message{Role: model.RoleAssistant},
		}},
	}); err != nil {
		t.Fatalf("Append empty message error = %v", err)
	}

	pack, err := rt.PackContext(ctx, PackContextRequest{
		Window: recent.WindowRequest{Scope: testScope("conv-1")},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.Window.Messages) != 1 {
		t.Fatalf("Window.Messages len = %d, want 1", len(pack.Window.Messages))
	}
	if len(pack.Items) != 0 {
		t.Fatalf("Items = %+v, want no rendered items for empty content", pack.Items)
	}
}

func TestWholeDocumentChunkerSkipsWhitespaceDocuments(t *testing.T) {
	chunks, err := (WholeDocumentChunker{}).ChunkDocument(context.Background(), DocumentChunkInput{
		Document: sourcedocument.Document{
			Content: " \n\t ",
		},
	})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if chunks != nil {
		t.Fatalf("ChunkDocument() chunks = %+v, want nil", chunks)
	}
}

func TestWholeDocumentChunkerSetsLayerSignatureAndObservedAt(t *testing.T) {
	updatedAt := time.Date(2026, 6, 10, 10, 30, 0, 0, time.UTC)
	chunks, err := (WholeDocumentChunker{
		Layer: viewdocument.Layer{
			Name:    "custom",
			Version: "v2",
		},
		TransformSignature: "custom:v2",
	}).ChunkDocument(context.Background(), DocumentChunkInput{
		View:  views.Descriptor{ID: viewdocument.DefaultChunksID},
		Scope: testDocumentScope("dataset-1"),
		Document: sourcedocument.Document{
			DatasetID:   "dataset-1",
			ID:          "doc-1",
			Content:     "hello world",
			Version:     7,
			ContentHash: "sha256:hello",
			UpdatedAt:   updatedAt,
		},
	})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("ChunkDocument() chunks len = %d, want 1", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Layer.TransformSignature != "custom:v2" {
		t.Fatalf("Layer.TransformSignature = %q, want custom:v2", chunk.Layer.TransformSignature)
	}
	if got := chunk.Signature.SourceRevisions[0].ObservedAt; !got.Equal(updatedAt) {
		t.Fatalf("ObservedAt = %v, want %v", got, updatedAt)
	}
	if err := chunk.Validate(); err != nil {
		t.Fatalf("chunk Validate() error = %v", err)
	}
}

func TestRequiredUnsupportedCapabilitiesReturnNotAvailable(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []compiler.CapabilitySpec
	}{
		{
			name: "entity profile",
			capabilities: []compiler.CapabilitySpec{
				{Capability: compiler.CapabilityObservationLedger, Required: true},
				{Capability: compiler.CapabilityFactLedger, Required: true},
				{Capability: compiler.CapabilityFactGraph, Required: true},
				{Capability: compiler.CapabilityEntityProfile, Required: true},
			},
		},
		{
			name: "entity timeline",
			capabilities: []compiler.CapabilitySpec{
				{Capability: compiler.CapabilityObservationLedger, Required: true},
				{Capability: compiler.CapabilityFactLedger, Required: true},
				{Capability: compiler.CapabilityFactGraph, Required: true},
				{Capability: compiler.CapabilityEntityTimeline, Required: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembly := compileAssembly(
				t,
				[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
				tt.capabilities,
				nil,
			)
			_, err := New(Deps{
				Assembly:             assembly,
				ObservationExtractor: &fakeObservationExtractor{},
			})
			if err == nil || !errdefs.IsNotAvailable(err) {
				t.Fatalf("New() err = %v, want NotAvailable", err)
			}
		})
	}
}

func TestProjectionWriterRequiresExplicitIndex(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceDocumentStore, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityDocumentChunks, Required: true}},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilityDocumentChunks, Namespace: "doc_chunks", Required: true}},
	)

	_, err := New(Deps{
		Assembly:        assembly,
		DocumentStore:   newDocumentStore(),
		ChunkStore:      newChunkStore(),
		DocumentChunker: &fakeChunker{},
	})
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "Index") {
		t.Fatalf("New() err = %v, want validation error mentioning Index", err)
	}
}

func TestInjectedIndexIsNotClosedByRuntimeClose(t *testing.T) {
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{{Capability: compiler.CapabilityRecentWindow, Required: true}},
		nil,
	)
	index := &closeTrackingIndex{}
	rt, err := New(Deps{
		Assembly:     assembly,
		MessageStore: newMessageStore(),
		Index:        index,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if index.closed {
		t.Fatal("injected index was closed; want Executor.Close to leave it open")
	}
}

func newExecutorDeps(t *testing.T, assembly compiler.Assembly) Deps {
	t.Helper()
	ws := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(ws, "retrieval"))
	if err != nil {
		t.Fatalf("create retrieval index error = %v", err)
	}
	return Deps{
		Assembly:         assembly,
		MessageStore:     sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/message")),
		DocumentStore:    sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/document")),
		SummaryStore:     recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(ws, "views/summary_dag")),
		ChunkStore:       viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(ws, "views/document_chunks")),
		ObservationStore: viewobservation.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/observation_ledger")),
		FactStore:        fact.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_ledger")),
		FactGraphStore:   fact.NewGraphWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_graph")),
		Index:            index,
	}
}

func newMessageStore() sourcemessage.Store {
	return sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "sources/message"))
}

func newDocumentStore() sourcedocument.Store {
	return sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "sources/document"))
}

func newSummaryStore() recent.SummaryStore {
	return recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/summary_dag"))
}

func newChunkStore() viewdocument.ChunkStore {
	return viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/document_chunks"))
}

func newObservationStore() viewobservation.Store {
	return viewobservation.NewLedgerWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/observation_ledger"))
}

func newFactStore() fact.Store {
	return fact.NewLedgerWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/fact_ledger"))
}

func newFactGraphStore() fact.GraphStore {
	return fact.NewGraphWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/fact_graph"))
}

func compileAssembly(t *testing.T, sources []compiler.SourceSpec, capabilities []compiler.CapabilitySpec, projections []compiler.ProjectionRequest) compiler.Assembly {
	t.Helper()
	assembly, err := compiler.Compile(compiler.Spec{
		Sources:      sources,
		Capabilities: capabilities,
		Projections:  projections,
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return assembly
}

func assertNamespace(t *testing.T, rt *Executor, capability compiler.Capability, want string) {
	t.Helper()
	got, ok := rt.ProjectionNamespace(capability)
	if !ok {
		t.Fatalf("ProjectionNamespace(%q) ok = false", capability)
	}
	if got != want {
		t.Fatalf("ProjectionNamespace(%q) = %q, want %q", capability, got, want)
	}
}

func messageWithText(text string) sourcemessage.Message {
	return sourcemessage.Message{
		Message: model.Message{
			Role: model.RoleUser,
			Parts: []model.Part{{
				Type: model.PartText,
				Text: text,
			}},
		},
	}
}

func testScope(conversationID string) views.Scope {
	return views.Scope{RuntimeID: "runtime-1", UserID: "user-1", ConversationID: conversationID}
}

func testDocumentScope(datasetID string) views.Scope {
	scope := testScope("conv-1")
	scope.DatasetID = datasetID
	return scope
}

type fakeChunker struct {
	calls int
}

func (f *fakeChunker) ChunkDocument(ctx context.Context, input DocumentChunkInput) ([]viewdocument.Chunk, error) {
	f.calls++
	return (WholeDocumentChunker{}).ChunkDocument(ctx, input)
}

type fakeSummarizer struct {
	calls int
}

func (f *fakeSummarizer) Summarize(_ context.Context, input SummaryInput) ([]recent.SummaryNode, error) {
	f.calls++
	if len(input.Window.Messages) == 0 {
		return nil, nil
	}
	sourceRefs := input.Window.SourceRefs
	return []recent.SummaryNode{{
		ID:         "summary-1",
		Scope:      input.Scope,
		SourceRefs: sourceRefs,
		Summary:    "runtime summary about memory",
		Level:      1,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    messageRevisions(input.Window.Messages, sourceRefs),
			TransformSignature: "fake-summary:v1",
		},
	}}, nil
}

type fakeObservationExtractor struct {
	calls int
}

func (f *fakeObservationExtractor) ExtractObservations(_ context.Context, input ObservationInput) ([]viewobservation.Observation, error) {
	f.calls++
	if len(input.Window.Messages) == 0 {
		return nil, nil
	}
	sourceRefs := input.Window.SourceRefs
	return []viewobservation.Observation{{
		ID:         "obs-1",
		Scope:      input.Scope,
		Subject:    "user:ada",
		Predicate:  "likes",
		Object:     "tea",
		Confidence: 0.9,
		SourceRefs: sourceRefs,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    messageRevisions(input.Window.Messages, sourceRefs),
			TransformSignature: "fake-observation:v1",
		},
	}}, nil
}

type fakeFactReconciler struct {
	calls int
}

func (f *fakeFactReconciler) ReconcileFacts(_ context.Context, input FactReconcileInput) ([]fact.Fact, error) {
	f.calls++
	if len(input.Observations) == 0 {
		return nil, nil
	}
	obs := input.Observations[0]
	return []fact.Fact{{
		ID:         "fact-1",
		Scope:      obs.Scope,
		Subject:    obs.Subject,
		Predicate:  obs.Predicate,
		Object:     obs.Object,
		Status:     fact.FactActive,
		Confidence: obs.Confidence,
		ObservationRefs: []fact.ObservationRef{{
			ObservationID: obs.ID,
		}},
		SourceRefs: obs.SourceRefs,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			UpstreamViewRefs: []views.UpstreamViewRef{{
				ViewID:          obs.Signature.ViewID,
				OutputSignature: obs.Signature.TransformSignature,
				RecordKey:       obs.ID,
			}},
			TransformSignature: "fake-fact:v1",
			DiagnosticSignatures: map[string]string{
				"reconciler": "fake-fact:v1",
			},
		},
	}}, nil
}

type fakeFactGraphBuilder struct {
	calls int
}

func (f *fakeFactGraphBuilder) BuildFactGraph(_ context.Context, input FactGraphInput) (FactGraphOutput, error) {
	f.calls++
	if len(input.Facts) == 0 {
		return FactGraphOutput{}, nil
	}
	record := input.Facts[0]
	sourceRefs := record.SourceRefs
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	signature := views.ViewSignature{
		ViewID: input.View.ID,
		UpstreamViewRefs: []views.UpstreamViewRef{{
			ViewID:          record.Signature.ViewID,
			OutputSignature: record.Signature.TransformSignature,
			RecordKey:       string(record.ID),
		}},
		TransformSignature: "fake-fact-graph:v1",
		DiagnosticSignatures: map[string]string{
			"projector": "fake-fact-graph:v1",
		},
	}
	return FactGraphOutput{
		Nodes: []fact.Node{
			{
				ID:         "node-subject",
				Scope:      record.Scope,
				Kind:       fact.NodeEntity,
				Label:      "Ada",
				Aliases:    []string{record.Subject},
				FactRefs:   factRefs,
				SourceRefs: sourceRefs,
				Signature:  signature,
			},
			{
				ID:         "node-object",
				Scope:      record.Scope,
				Kind:       fact.NodeValue,
				Label:      record.Object,
				FactRefs:   factRefs,
				SourceRefs: sourceRefs,
				Signature:  signature,
			},
		},
		Edges: []fact.Edge{{
			ID:         "edge-1",
			Scope:      record.Scope,
			From:       "node-subject",
			To:         "node-object",
			Predicate:  record.Predicate,
			Status:     fact.FactActive,
			Confidence: record.Confidence,
			FactRefs:   factRefs,
			SourceRefs: sourceRefs,
			Signature:  signature,
		}},
	}, nil
}

func messageRevisions(messages []sourcemessage.Message, refs []views.SourceRef) []views.SourceRevision {
	revisions := make([]views.SourceRevision, 0, len(messages))
	for i, msg := range messages {
		revisions = append(revisions, views.SourceRevision{
			Kind:      views.SourceMessage,
			SourceKey: refs[i].StableKey(),
			Revision:  strconv.FormatUint(msg.Seq, 10),
		})
	}
	return revisions
}

type closeTrackingIndex struct {
	closed bool
}

func (i *closeTrackingIndex) Upsert(context.Context, string, []retrieval.Doc) error {
	return nil
}

func (i *closeTrackingIndex) Delete(context.Context, string, []string) error {
	return nil
}

func (i *closeTrackingIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}

func (i *closeTrackingIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}

func (i *closeTrackingIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{}
}

func (i *closeTrackingIndex) Close() error {
	i.closed = true
	return nil
}
