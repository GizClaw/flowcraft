package executor

import (
	"context"
	"errors"
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
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
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

func TestReconcileFactsProvidesCurrentActiveFactsInScope(t *testing.T) {
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

	reconciler := &fakeFactReconciler{}
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = reconciler
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(fact current runtime) error = %v", err)
	}
	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages:       []sourcemessage.Message{messageWithText("Ada likes tea.")},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}

	scope := testScope("conv-1")
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: scope}, scope, "")
	if err != nil {
		t.Fatalf("ExtractObservations() error = %v", err)
	}
	if _, err := deps.FactStore.Put(ctx, runtimeFact("current-active", scope, fact.FactActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.FactStore.Put(ctx, runtimeFact("current-retracted", scope, fact.FactRetracted)); err != nil {
		t.Fatal(err)
	}
	otherScope := testScope("conv-2")
	if _, err := deps.FactStore.Put(ctx, runtimeFact("other-active", otherScope, fact.FactActive)); err != nil {
		t.Fatal(err)
	}

	if _, err := rt.ReconcileFacts(ctx, observations); err != nil {
		t.Fatalf("ReconcileFacts() error = %v", err)
	}
	if reconciler.lastInput.Scope != scope {
		t.Fatalf("reconciler scope = %+v, want %+v", reconciler.lastInput.Scope, scope)
	}
	if len(reconciler.lastInput.Current) != 1 || reconciler.lastInput.Current[0].ID != "current-active" {
		t.Fatalf("reconciler current = %+v, want only scoped active fact", reconciler.lastInput.Current)
	}
}

func TestFactProjectionAndSearchAreActiveOnly(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
		},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilityFactLedger, Namespace: "facts", Required: true}},
	)

	scope := testScope("conv-1")
	reconciler := &fakeFactReconciler{
		output: []fact.Fact{
			runtimeFact("active-fact", scope, fact.FactActive),
			runtimeFact("retracted-fact", scope, fact.FactRetracted),
			runtimeFact("superseded-fact", scope, fact.FactSuperseded),
			runtimeFact("conflict-fact", scope, fact.FactConflict),
		},
	}
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = reconciler
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(fact search runtime) error = %v", err)
	}

	stored, err := rt.ReconcileFacts(ctx, []viewobservation.Observation{runtimeObservation("obs-1", scope)})
	if err != nil {
		t.Fatalf("ReconcileFacts() error = %v", err)
	}
	if len(stored) != 4 {
		t.Fatalf("ReconcileFacts() stored = %+v, want all ledger facts", stored)
	}
	for _, id := range []fact.FactID{"retracted-fact", "superseded-fact", "conflict-fact"} {
		got, ok, err := deps.FactStore.Get(ctx, id)
		if err != nil || !ok || got.ID != id {
			t.Fatalf("ledger Get(%q) = %+v ok=%v err=%v, want stored non-active fact", id, got, ok, err)
		}
	}

	results, err := rt.SearchFacts(ctx, retrieval.SearchRequest{QueryText: "user:ada likes tea", TopK: 10})
	if err != nil {
		t.Fatalf("SearchFacts() error = %v", err)
	}
	if len(results.Hits) != 1 || results.Hits[0].Fact.ID != "active-fact" {
		t.Fatalf("SearchFacts() hits = %+v, want only active fact", results.Hits)
	}
}

func TestFactGraphAndEntityBuildersReceiveOnlyActiveFacts(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
			{Capability: compiler.CapabilityEntityProfile, Required: true},
			{Capability: compiler.CapabilityEntityTimeline, Required: true},
		},
		nil,
	)

	graphBuilder := &fakeFactGraphBuilder{}
	profileBuilder := &fakeEntityProfileBuilder{}
	timelineBuilder := &fakeEntityTimelineBuilder{}
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = graphBuilder
	deps.EntityProfileBuilder = profileBuilder
	deps.EntityTimelineBuilder = timelineBuilder
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(active-only builders runtime) error = %v", err)
	}

	scope := testScope("conv-entity")
	scope.EntityID = "user:ada"
	facts := []fact.Fact{
		runtimeFact("active-fact", scope, fact.FactActive),
		runtimeFact("retracted-fact", scope, fact.FactRetracted),
		runtimeFact("superseded-fact", scope, fact.FactSuperseded),
		runtimeFact("conflict-fact", scope, fact.FactConflict),
	}
	graph, err := rt.BuildFactGraph(ctx, facts)
	if err != nil {
		t.Fatalf("BuildFactGraph() error = %v", err)
	}
	if len(graphBuilder.lastInput.Facts) != 1 || graphBuilder.lastInput.Facts[0].ID != "active-fact" {
		t.Fatalf("graph builder facts = %+v, want only active fact", graphBuilder.lastInput.Facts)
	}

	if _, err := rt.BuildEntityProfiles(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph}); err != nil {
		t.Fatalf("BuildEntityProfiles() error = %v", err)
	}
	if len(profileBuilder.lastInput.Facts) != 1 || profileBuilder.lastInput.Facts[0].ID != "active-fact" {
		t.Fatalf("profile builder facts = %+v, want only active fact", profileBuilder.lastInput.Facts)
	}

	if _, err := rt.BuildEntityTimeline(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph}); err != nil {
		t.Fatalf("BuildEntityTimeline() error = %v", err)
	}
	if len(timelineBuilder.lastInput.Facts) != 1 || timelineBuilder.lastInput.Facts[0].ID != "active-fact" {
		t.Fatalf("timeline builder facts = %+v, want only active fact", timelineBuilder.lastInput.Facts)
	}
}

func TestRuntimeEntityProfileAndTimelineVerticalSlice(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityRecentWindow, Required: true},
			{Capability: compiler.CapabilityObservationLedger, Required: true},
			{Capability: compiler.CapabilityFactLedger, Required: true},
			{Capability: compiler.CapabilityFactGraph, Required: true},
			{Capability: compiler.CapabilityEntityProfile, Required: true},
			{Capability: compiler.CapabilityEntityTimeline, Required: true},
		},
		[]compiler.ProjectionRequest{
			{Capability: compiler.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
			{Capability: compiler.CapabilityEntityProfile, Namespace: "entity_profiles", Required: true},
			{Capability: compiler.CapabilityEntityTimeline, Namespace: "entity_timeline", Required: true},
		},
	)

	profileBuilder := &fakeEntityProfileBuilder{}
	timelineBuilder := &fakeEntityTimelineBuilder{}
	deps := newExecutorDeps(t, assembly)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.EntityProfileBuilder = profileBuilder
	deps.EntityTimelineBuilder = timelineBuilder
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(entity vertical slice runtime) error = %v", err)
	}
	assertNamespace(t, rt, compiler.CapabilityEntityProfile, "entity_profiles")
	assertNamespace(t, rt, compiler.CapabilityEntityTimeline, "entity_timeline")

	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-entity",
		Messages: []sourcemessage.Message{
			messageWithText("Ada likes tea."),
		},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}

	scope := testScope("conv-entity")
	scope.EntityID = "user:ada"
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: scope}, scope, "")
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

	profiles, err := rt.BuildEntityProfiles(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph})
	if err != nil {
		t.Fatalf("BuildEntityProfiles() error = %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != "profile-user:ada" || profileBuilder.calls != 1 {
		t.Fatalf("BuildEntityProfiles() profiles = %+v calls=%d, want one built profile", profiles, profileBuilder.calls)
	}
	profileResults, err := rt.SearchEntityProfiles(ctx, retrieval.SearchRequest{QueryText: "Ada tea profile", TopK: 5})
	if err != nil {
		t.Fatalf("SearchEntityProfiles() error = %v", err)
	}
	if len(profileResults.Hits) != 1 || profileResults.Hits[0].Profile.ID != profiles[0].ID {
		t.Fatalf("SearchEntityProfiles() hits = %+v, want hydrated profile", profileResults.Hits)
	}

	events, err := rt.BuildEntityTimeline(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph})
	if err != nil {
		t.Fatalf("BuildEntityTimeline() error = %v", err)
	}
	if len(events) != 1 || events[0].ID != "event-user:ada" || timelineBuilder.calls != 1 {
		t.Fatalf("BuildEntityTimeline() events = %+v calls=%d, want one built event", events, timelineBuilder.calls)
	}
	timelineResults, err := rt.SearchEntityTimeline(ctx, retrieval.SearchRequest{QueryText: "Ada tea event", TopK: 5})
	if err != nil {
		t.Fatalf("SearchEntityTimeline() error = %v", err)
	}
	if len(timelineResults.Hits) != 1 || timelineResults.Hits[0].Event.ID != events[0].ID {
		t.Fatalf("SearchEntityTimeline() hits = %+v, want hydrated event", timelineResults.Hits)
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

func TestPackContextPackerCanSeeTypedInputAndFilterItems(t *testing.T) {
	ctx := context.Background()
	packer := &fakeContextPacker{}
	rt, req := setupContextPackerRuntime(t, ctx, packer)

	packer.fn = func(input ContextPackInput) (ContextPackOutput, error) {
		if input.Scope != req.Scope {
			t.Fatalf("packer Scope = %+v, want %+v", input.Scope, req.Scope)
		}
		if input.Query != req.Query {
			t.Fatalf("packer Query = %q, want %q", input.Query, req.Query)
		}
		if len(input.Window.Messages) != 2 {
			t.Fatalf("packer Window.Messages len = %d, want 2", len(input.Window.Messages))
		}
		if len(input.SummaryHits) != 1 || len(input.DocumentHits) != 1 || len(input.ObservationHits) != 1 || len(input.FactHits) != 1 {
			t.Fatalf("packer core hits = summary:%d document:%d observation:%d fact:%d, want one each", len(input.SummaryHits), len(input.DocumentHits), len(input.ObservationHits), len(input.FactHits))
		}
		if len(input.FactGraphHits) == 0 || len(input.EntityProfileHits) != 1 || len(input.EntityTimelineHits) != 1 {
			t.Fatalf("packer graph/entity hits = graph:%d profile:%d timeline:%d, want graph plus one entity hit each", len(input.FactGraphHits), len(input.EntityProfileHits), len(input.EntityTimelineHits))
		}
		if len(input.Items) < 3 {
			t.Fatalf("packer Items len = %d, want candidates", len(input.Items))
		}
		return ContextPackOutput{Items: []ContextItem{input.Items[2], input.Items[0]}}, nil
	}

	pack, err := rt.PackContext(ctx, req)
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if packer.calls != 1 {
		t.Fatalf("packer calls = %d, want 1", packer.calls)
	}
	if got, want := len(pack.Items), 2; got != want {
		t.Fatalf("Items len = %d, want %d: %+v", got, want, pack.Items)
	}
	if pack.Items[0].Kind != ContextItemSummaryNode || pack.Items[1].Kind != ContextItemRecentMessage {
		t.Fatalf("Items order = %q then %q, want hook-selected summary then recent", pack.Items[0].Kind, pack.Items[1].Kind)
	}
	if len(pack.SummaryHits) != 1 || len(pack.DocumentHits) != 1 || len(pack.EntityProfileHits) != 1 || len(pack.EntityTimelineHits) != 1 {
		t.Fatalf("typed hits changed after packing: summary:%d document:%d profile:%d timeline:%d", len(pack.SummaryHits), len(pack.DocumentHits), len(pack.EntityProfileHits), len(pack.EntityTimelineHits))
	}
}

func TestPackContextPackerErrorReturnsError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("packer failed")
	packer := &fakeContextPacker{
		fn: func(ContextPackInput) (ContextPackOutput, error) {
			return ContextPackOutput{}, wantErr
		},
	}
	rt, req := setupContextPackerRuntime(t, ctx, packer)

	_, err := rt.PackContext(ctx, req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("PackContext() err = %v, want %v", err, wantErr)
	}
}

func TestPackContextPackerInputMutationDoesNotPolluteTypedHits(t *testing.T) {
	ctx := context.Background()
	packer := &fakeContextPacker{}
	rt, req := setupContextPackerRuntime(t, ctx, packer)

	packer.fn = func(input ContextPackInput) (ContextPackOutput, error) {
		if len(input.Items) == 0 || len(input.FactHits) == 0 || len(input.DocumentHits) == 0 {
			t.Fatalf("packer input missing candidates: %+v", input)
		}
		input.Items[0].Text = "mutated item"
		for i := range input.Items {
			if input.Items[i].Fact != nil {
				input.Items[i].Fact.Subject = "mutated item fact"
				break
			}
		}
		input.FactHits[0].Fact.Subject = "mutated fact hit"
		input.DocumentHits[0].Chunk.Text = "mutated document hit"
		return ContextPackOutput{}, nil
	}

	pack, err := rt.PackContext(ctx, req)
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(pack.Items) != 0 {
		t.Fatalf("Items len = %d, want hook-filtered empty items", len(pack.Items))
	}
	if got := pack.FactHits[0].Fact.Subject; got != "user:ada" {
		t.Fatalf("FactHits[0].Fact.Subject = %q, want original", got)
	}
	if got := pack.DocumentHits[0].Chunk.Text; got != "chunkable document evidence about runtime memory" {
		t.Fatalf("DocumentHits[0].Chunk.Text = %q, want original", got)
	}
}

func setupContextPackerRuntime(t *testing.T, ctx context.Context, packer ContextPacker) (*Executor, PackContextRequest) {
	t.Helper()
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
			{Capability: compiler.CapabilityEntityProfile, Required: true},
			{Capability: compiler.CapabilityEntityTimeline, Required: true},
		},
		[]compiler.ProjectionRequest{
			{Capability: compiler.CapabilitySummaryDAG, Namespace: "summary_nodes", Required: true},
			{Capability: compiler.CapabilityDocumentChunks, Namespace: "doc_chunks", Required: true},
			{Capability: compiler.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: compiler.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: compiler.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
			{Capability: compiler.CapabilityEntityProfile, Namespace: "entity_profiles", Required: true},
			{Capability: compiler.CapabilityEntityTimeline, Namespace: "entity_timeline", Required: true},
		},
	)
	deps := newExecutorDeps(t, assembly)
	deps.DocumentChunker = &fakeChunker{}
	deps.Summarizer = &fakeSummarizer{}
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
	deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
	deps.ContextPacker = packer
	rt, err := New(deps)
	if err != nil {
		t.Fatalf("New(context packer runtime) error = %v", err)
	}

	scope := testScope("conv-packer")
	scope.DatasetID = "dataset-1"
	scope.EntityID = "user:ada"
	if _, err := rt.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages: []sourcemessage.Message{
			messageWithText("Ada likes tea."),
			messageWithText("The project summary should mention memory runtime."),
		},
	}); err != nil {
		t.Fatalf("Append messages error = %v", err)
	}
	if _, err := rt.DocumentStore().Put(ctx, sourcedocument.PutRequest{
		Document: sourcedocument.Document{
			DatasetID: scope.DatasetID,
			ID:        "doc-1",
			Content:   "chunkable document evidence about runtime memory",
		},
	}); err != nil {
		t.Fatalf("Put document error = %v", err)
	}
	if _, err := rt.IndexDocument(ctx, scope, "doc-1", ""); err != nil {
		t.Fatalf("IndexDocument() error = %v", err)
	}
	if _, err := rt.BuildSummaryDAG(ctx, recent.WindowRequest{Scope: scope}, ""); err != nil {
		t.Fatalf("BuildSummaryDAG() error = %v", err)
	}
	observations, err := rt.ExtractObservations(ctx, recent.WindowRequest{Scope: scope}, scope, "")
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
	if _, err := rt.BuildEntityProfiles(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph}); err != nil {
		t.Fatalf("BuildEntityProfiles() error = %v", err)
	}
	if _, err := rt.BuildEntityTimeline(ctx, EntityBuildInput{Scope: scope, Facts: facts, Graph: graph}); err != nil {
		t.Fatalf("BuildEntityTimeline() error = %v", err)
	}

	summarySearch := retrieval.SearchRequest{QueryText: "runtime summary", TopK: 5}
	documentSearch := retrieval.SearchRequest{QueryText: "chunkable", TopK: 5}
	observationSearch := retrieval.SearchRequest{QueryText: "likes tea", TopK: 5}
	factSearch := retrieval.SearchRequest{QueryText: "Ada likes tea", TopK: 5}
	factGraphSearch := retrieval.SearchRequest{QueryText: "Ada tea likes", TopK: 10}
	entityProfileSearch := retrieval.SearchRequest{QueryText: "Ada tea profile", TopK: 5}
	entityTimelineSearch := retrieval.SearchRequest{QueryText: "Ada tea event", TopK: 5}
	return rt, PackContextRequest{
		Scope:                scope,
		Query:                "Ada tea hook query",
		Window:               recent.WindowRequest{Scope: scope},
		SummarySearch:        &summarySearch,
		DocumentSearch:       &documentSearch,
		ObservationSearch:    &observationSearch,
		FactSearch:           &factSearch,
		FactGraphSearch:      &factGraphSearch,
		EntityProfileSearch:  &entityProfileSearch,
		EntityTimelineSearch: &entityTimelineSearch,
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

func TestNewRequiredEntityCapabilitiesMissingDependenciesFail(t *testing.T) {
	tests := []struct {
		name string
		deps func(Deps) Deps
		want string
	}{
		{
			name: "entity profile store",
			deps: func(deps Deps) Deps {
				deps.EntityProfileStore = nil
				deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
				deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
				return deps
			},
			want: "EntityProfileStore",
		},
		{
			name: "entity profile builder",
			deps: func(deps Deps) Deps {
				deps.EntityProfileBuilder = nil
				deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
				return deps
			},
			want: "EntityProfileBuilder",
		},
		{
			name: "entity timeline store",
			deps: func(deps Deps) Deps {
				deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
				deps.EntityTimelineStore = nil
				deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
				return deps
			},
			want: "EntityTimelineStore",
		},
		{
			name: "entity timeline builder",
			deps: func(deps Deps) Deps {
				deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
				deps.EntityTimelineBuilder = nil
				return deps
			},
			want: "EntityTimelineBuilder",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembly := compileAssembly(
				t,
				[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog, Required: true}},
				[]compiler.CapabilitySpec{
					{Capability: compiler.CapabilityObservationLedger, Required: true},
					{Capability: compiler.CapabilityFactLedger, Required: true},
					{Capability: compiler.CapabilityFactGraph, Required: true},
					{Capability: compiler.CapabilityEntityProfile, Required: true},
					{Capability: compiler.CapabilityEntityTimeline, Required: true},
				},
				nil,
			)
			deps := newExecutorDeps(t, assembly)
			deps.ObservationExtractor = &fakeObservationExtractor{}
			deps.FactReconciler = &fakeFactReconciler{}
			deps.FactGraphBuilder = &fakeFactGraphBuilder{}
			deps = tt.deps(deps)
			_, err := New(deps)
			if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() err = %v, want validation error mentioning %s", err, tt.want)
			}
		})
	}
}

func TestOptionalEntityProjectionWithoutBuilderSkipsWriterAndIndex(t *testing.T) {
	ctx := context.Background()
	assembly := compileAssembly(
		t,
		[]compiler.SourceSpec{{Kind: compiler.SourceMessageLog}},
		[]compiler.CapabilitySpec{
			{Capability: compiler.CapabilityObservationLedger},
			{Capability: compiler.CapabilityFactLedger},
			{Capability: compiler.CapabilityFactGraph},
			{Capability: compiler.CapabilityEntityProfile},
		},
		[]compiler.ProjectionRequest{{Capability: compiler.CapabilityEntityProfile, Namespace: "entity_profiles"}},
	)

	rt, err := New(Deps{Assembly: assembly})
	if err != nil {
		t.Fatalf("New(optional entity projection without builder) error = %v", err)
	}
	if rt.RetrievalIndex() != nil {
		t.Fatal("RetrievalIndex() != nil, want no index when entity profile flow is not configured")
	}
	if rt.writers[compiler.CapabilityEntityProfile] != nil {
		t.Fatal("entity profile writer configured, want nil")
	}

	_, err = rt.SearchEntityProfiles(ctx, retrieval.SearchRequest{QueryText: "entity", TopK: 1})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("SearchEntityProfiles() err = %v, want NotAvailable", err)
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
		Assembly:            assembly,
		MessageStore:        sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/message")),
		DocumentStore:       sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/document")),
		SummaryStore:        recent.NewSummaryWorkspaceStore(sdkworkspace.Sub(ws, "views/summary_dag")),
		ChunkStore:          viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(ws, "views/document_chunks")),
		ObservationStore:    viewobservation.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/observation_ledger")),
		FactStore:           fact.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_ledger")),
		FactGraphStore:      fact.NewGraphWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_graph")),
		EntityProfileStore:  viewentity.NewProfileWorkspaceStore(sdkworkspace.Sub(ws, "views/entity_profile")),
		EntityTimelineStore: viewentity.NewTimelineWorkspaceStore(sdkworkspace.Sub(ws, "views/entity_timeline")),
		Index:               index,
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

func newEntityProfileStore() viewentity.ProfileStore {
	return viewentity.NewProfileWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/entity_profile"))
}

func newEntityTimelineStore() viewentity.TimelineStore {
	return viewentity.NewTimelineWorkspaceStore(sdkworkspace.Sub(sdkworkspace.NewMemWorkspace(), "views/entity_timeline"))
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

func runtimeObservation(id string, scope views.Scope) viewobservation.Observation {
	sourceRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: scope.ConversationID,
			MessageID:      "message-" + id,
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
	return viewobservation.Observation{
		ID:         id,
		Scope:      scope,
		Subject:    "user:ada",
		Predicate:  "likes",
		Object:     "tea",
		Confidence: 0.9,
		SourceRefs: []views.SourceRef{sourceRef},
		Signature: views.ViewSignature{
			ViewID:             views.ID("observation-ledger"),
			TransformSignature: "runtime-observation:v1",
		},
	}
}

func runtimeFact(id fact.FactID, scope views.Scope, status fact.FactStatus) fact.Fact {
	obs := runtimeObservation("obs-"+string(id), scope)
	return fact.Fact{
		ID:         id,
		Scope:      scope,
		Subject:    obs.Subject,
		Predicate:  obs.Predicate,
		Object:     obs.Object,
		Status:     status,
		Confidence: obs.Confidence,
		ObservationRefs: []fact.ObservationRef{{
			ObservationID: obs.ID,
			ScopeKind:     "conversation",
			ScopeID:       scope.ConversationID,
		}},
		SourceRefs: obs.SourceRefs,
		Signature: views.ViewSignature{
			ViewID: views.ID("fact-ledger"),
			UpstreamViewRefs: []views.UpstreamViewRef{{
				ViewID:          obs.Signature.ViewID,
				OutputSignature: obs.Signature.TransformSignature,
				RecordKey:       obs.ID,
			}},
			TransformSignature: "runtime-fact:v1",
		},
	}
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
	calls     int
	lastInput FactReconcileInput
	output    []fact.Fact
}

func (f *fakeFactReconciler) ReconcileFacts(_ context.Context, input FactReconcileInput) ([]fact.Fact, error) {
	f.calls++
	f.lastInput = input
	if f.output != nil {
		return f.output, nil
	}
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
	calls     int
	lastInput FactGraphInput
}

func (f *fakeFactGraphBuilder) BuildFactGraph(_ context.Context, input FactGraphInput) (FactGraphOutput, error) {
	f.calls++
	f.lastInput = input
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

type fakeEntityProfileBuilder struct {
	calls     int
	lastInput EntityProfileInput
}

func (f *fakeEntityProfileBuilder) BuildEntityProfiles(_ context.Context, input EntityProfileInput) ([]viewentity.ProfileRecord, error) {
	f.calls++
	f.lastInput = input
	if len(input.Facts) == 0 {
		return nil, nil
	}
	record := input.Facts[0]
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	return []viewentity.ProfileRecord{{
		ID:         viewentity.ProfileID("profile-" + input.Scope.EntityID),
		Scope:      input.Scope,
		Label:      "Ada",
		Summary:    "Ada likes tea profile",
		Slots:      []viewentity.Slot{{Name: "likes", Value: "tea", Confidence: 0.9, FactRefs: factRefs}},
		FactRefs:   factRefs,
		SourceRefs: record.SourceRefs,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			UpstreamViewRefs: []views.UpstreamViewRef{{
				ViewID:          upstreamEntityViewID(input.Graph, record),
				OutputSignature: upstreamEntitySignature(input.Graph, record),
				RecordKey:       string(record.ID),
			}},
			TransformSignature: "fake-entity-profile:v1",
		},
	}}, nil
}

type fakeEntityTimelineBuilder struct {
	calls     int
	lastInput EntityTimelineInput
}

func (f *fakeEntityTimelineBuilder) BuildEntityTimeline(_ context.Context, input EntityTimelineInput) ([]viewentity.Event, error) {
	f.calls++
	f.lastInput = input
	if len(input.Facts) == 0 {
		return nil, nil
	}
	record := input.Facts[0]
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	return []viewentity.Event{{
		ID:          viewentity.EventID("event-" + input.Scope.EntityID),
		Scope:       input.Scope,
		Title:       "Ada likes tea event",
		Description: "Ada likes tea",
		FactRefs:    factRefs,
		SourceRefs:  record.SourceRefs,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			UpstreamViewRefs: []views.UpstreamViewRef{{
				ViewID:          upstreamEntityViewID(input.Graph, record),
				OutputSignature: upstreamEntitySignature(input.Graph, record),
				RecordKey:       string(record.ID),
			}},
			TransformSignature: "fake-entity-timeline:v1",
		},
	}}, nil
}

type fakeContextPacker struct {
	calls int
	input ContextPackInput
	fn    func(ContextPackInput) (ContextPackOutput, error)
}

func (f *fakeContextPacker) PackContext(_ context.Context, input ContextPackInput) (ContextPackOutput, error) {
	f.calls++
	f.input = cloneContextPackInput(input)
	if f.fn != nil {
		return f.fn(input)
	}
	return ContextPackOutput{Items: input.Items}, nil
}

func upstreamEntityViewID(graph FactGraphOutput, record fact.Fact) views.ID {
	if len(graph.Nodes) > 0 && graph.Nodes[0].Signature.ViewID != "" {
		return graph.Nodes[0].Signature.ViewID
	}
	return record.Signature.ViewID
}

func upstreamEntitySignature(graph FactGraphOutput, record fact.Fact) string {
	if len(graph.Nodes) > 0 && graph.Nodes[0].Signature.TransformSignature != "" {
		return graph.Nodes[0].Signature.TransformSignature
	}
	return record.Signature.TransformSignature
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
