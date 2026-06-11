package memory_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory"
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

func TestDecodeYAMLAndJSONCompileEquivalentSpec(t *testing.T) {
	yamlSpec := `
sources:
  - kind: message_log
    required: true
capabilities:
  - capability: recent_window
    required: true
  - capability: observation_ledger
    required: true
  - capability: fact_ledger
    purpose: durable claims
projections:
  - capability: observation_ledger
    namespace: observations
    required: true
write_stages:
  - name: extract_observations
    async: true
  - name: reconcile_facts
    async: true
read_stages:
  - name: load_recent_messages
  - name: retrieve_observations
  - name: pack_context
lifecycle:
  - name: readiness
  - name: queue_stats
diagnostics:
  - name: readiness
`
	jsonSpec := `{
  "sources": [{"kind": "message_log", "required": true}],
  "capabilities": [
    {"capability": "recent_window", "required": true},
    {"capability": "observation_ledger", "required": true},
    {"capability": "fact_ledger", "purpose": "durable claims"}
  ],
  "projections": [{"capability": "observation_ledger", "namespace": "observations", "required": true}],
  "write_stages": [{"name": "extract_observations", "async": true}, {"name": "reconcile_facts", "async": true}],
  "read_stages": [{"name": "load_recent_messages"}, {"name": "retrieve_observations"}, {"name": "pack_context"}],
  "lifecycle": [{"name": "readiness"}, {"name": "queue_stats"}],
  "diagnostics": [{"name": "readiness"}]
}`

	gotYAML, err := memory.Decode(strings.NewReader(yamlSpec))
	if err != nil {
		t.Fatalf("Decode yaml error = %v", err)
	}
	gotJSON, err := memory.Decode(strings.NewReader(jsonSpec))
	if err != nil {
		t.Fatalf("Decode json error = %v", err)
	}
	if !reflect.DeepEqual(gotYAML, gotJSON) {
		t.Fatalf("Decode yaml/json mismatch:\nyaml=%+v\njson=%+v", gotYAML, gotJSON)
	}
	var compilerSpec compiler.Spec = gotYAML
	var publicSpec memory.Spec = compilerSpec
	if !reflect.DeepEqual(publicSpec, gotYAML) {
		t.Fatalf("memory.Spec is not compiler.Spec alias-compatible:\npublic=%+v\ncompiler=%+v", publicSpec, compilerSpec)
	}
	if err := memory.Compile(gotYAML); err != nil {
		t.Fatalf("Compile yaml spec error = %v", err)
	}
	if err := memory.Compile(gotJSON); err != nil {
		t.Fatalf("Compile json spec error = %v", err)
	}
}

func TestDecodeInvalidProjectionReturnsCompileError(t *testing.T) {
	spec := `
sources:
  - kind: message_log
capabilities:
  - capability: recent_window
projections:
  - capability: observation_ledger
    namespace: observations
`
	_, err := memory.Decode(strings.NewReader(spec))
	if err == nil {
		t.Fatal("Decode invalid spec error = nil, want compile validation error")
	}
}

func TestCompileValidatesSpec(t *testing.T) {
	if err := memory.Compile(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		Projections: []memory.ProjectionSpec{{
			Capability: memory.CapabilityObservationLedger,
			Namespace:  "observations",
			Required:   true,
		}},
	}); err != nil {
		t.Fatalf("Compile error = %v", err)
	}
}

func TestMemoryFacadeAppendMessageAndPackContextFromYAMLStages(t *testing.T) {
	ctx := context.Background()
	specYAML := `
sources:
  - kind: message_log
    required: true
capabilities:
  - capability: recent_window
    required: true
  - capability: observation_ledger
    required: true
  - capability: fact_ledger
    required: true
  - capability: fact_graph
    required: true
projections:
  - capability: observation_ledger
    namespace: observations
    required: true
  - capability: fact_ledger
    namespace: facts
    required: true
  - capability: fact_graph
    namespace: fact_graph
    required: true
write_stages:
  - name: append_message
  - name: extract_observations
  - name: reconcile_facts
  - name: build_fact_graph
read_stages:
  - name: load_recent_messages
  - name: retrieve_observations
  - name: retrieve_facts
  - name: retrieve_fact_graph
  - name: pack_context
`
	spec, err := memory.Decode(strings.NewReader(specYAML))
	if err != nil {
		t.Fatalf("Decode high-level yaml spec error = %v", err)
	}
	extractor := &fakeObservationExtractor{}
	reconciler := &fakeFactReconciler{}
	graphBuilder := &fakeFactGraphBuilder{}
	deps := newDeps(t)
	deps.ObservationExtractor = extractor
	deps.FactReconciler = reconciler
	deps.FactGraphBuilder = graphBuilder

	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Fatalf("Close error = %v", err)
		}
	})
	if mem.MessageStore() == nil {
		t.Fatal("MessageStore() = nil, want injected store")
	}
	if mem.RetrievalIndex() == nil {
		t.Fatal("RetrievalIndex() = nil, want injected index")
	}
	assertNamespace(t, mem, memory.CapabilityObservationLedger, "observations")
	assertNamespace(t, mem, memory.CapabilityFactLedger, "facts")
	assertNamespace(t, mem, memory.CapabilityFactGraph, "fact_graph")
	plan := mem.Plan()
	if got, want := len(plan.Write), 4; got != want {
		t.Fatalf("Plan().Write len = %d, want %d", got, want)
	}
	if plan.Write[2].Name != "reconcile_facts" || plan.Write[2].Capability != memory.CapabilityFactLedger {
		t.Fatalf("Plan().Write[2] = %+v, want fact reconciliation stage with capability", plan.Write[2])
	}
	if got, want := len(plan.Lifecycle), 4; got != want {
		t.Fatalf("Plan().Lifecycle len = %d, want default lifecycle %d", got, want)
	}

	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    testScope("conv-1"),
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage error = %v", err)
	}
	if len(result.Observations) != 1 || result.Observations[0].ID != "obs-1" || extractor.calls != 1 {
		t.Fatalf("AppendMessage observations = %+v, calls=%d; want one adapted observation", result.Observations, extractor.calls)
	}
	if got := result.Observations[0].Scope; got != testScope("conv-1") {
		t.Fatalf("AppendMessage scope = %+v, want %+v", got, testScope("conv-1"))
	}
	if len(result.Facts) != 1 || result.Facts[0].ID != "fact-1" || reconciler.calls != 1 {
		t.Fatalf("AppendMessage facts = %+v, calls=%d; want one adapted fact", result.Facts, reconciler.calls)
	}
	if result.FactGraph == nil || len(result.FactGraph.Nodes) != 2 || len(result.FactGraph.Edges) != 1 || graphBuilder.calls != 1 {
		t.Fatalf("AppendMessage graph = %+v, calls=%d; want two nodes and one edge", result.FactGraph, graphBuilder.calls)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: testScope("conv-1"),
		Query: "Ada tea likes",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("PackContext error = %v", err)
	}
	if len(pack.Window.Messages) != 1 {
		t.Fatalf("PackContext Window.Messages len = %d, want 1", len(pack.Window.Messages))
	}
	if len(pack.ObservationHits) != 1 || pack.ObservationHits[0].Observation.ID != result.Observations[0].ID {
		t.Fatalf("PackContext ObservationHits = %+v, want hydrated observation", pack.ObservationHits)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.ID != result.Facts[0].ID {
		t.Fatalf("PackContext FactHits = %+v, want hydrated fact", pack.FactHits)
	}
	if len(pack.FactGraphHits) != len(result.FactGraph.Nodes)+len(result.FactGraph.Edges) {
		t.Fatalf("PackContext FactGraphHits len = %d, want %d: %+v", len(pack.FactGraphHits), len(result.FactGraph.Nodes)+len(result.FactGraph.Edges), pack.FactGraphHits)
	}
	if len(pack.Items) < 4 {
		t.Fatalf("PackContext Items = %+v, want recent plus stage-selected retrieval items", pack.Items)
	}
}

func TestMemoryFacadeDefaultsEmptyWriteAndReadStages(t *testing.T) {
	ctx := context.Background()
	spec := memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
			{Capability: memory.CapabilityFactLedger, Required: true},
			{Capability: memory.CapabilityFactGraph, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: memory.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: memory.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
		},
	}
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New default-stage memory error = %v", err)
	}

	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    testScope("conv-1"),
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage default stages error = %v", err)
	}
	if len(result.Observations) != 1 || len(result.Facts) != 1 || result.FactGraph == nil {
		t.Fatalf("AppendMessage default result = %+v, want observation, fact, and graph", result)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: testScope("conv-1"), Query: "Ada tea likes"})
	if err != nil {
		t.Fatalf("PackContext default stages error = %v", err)
	}
	if len(pack.ObservationHits) != 1 || len(pack.FactHits) != 1 || len(pack.FactGraphHits) == 0 {
		t.Fatalf("PackContext default hits = obs:%d facts:%d graph:%d, want all configured projections", len(pack.ObservationHits), len(pack.FactHits), len(pack.FactGraphHits))
	}
}

func TestMemoryFacadeImportDocumentStoresChunksAndPacksScopedContext(t *testing.T) {
	ctx := context.Background()
	chunker := &fakeDocumentChunker{}
	deps := newDocumentDeps(t)
	deps.DocumentChunker = chunker
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New document memory error = %v", err)
	}

	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"
	result, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			ID:      "doc-1",
			Content: "flowcraft document indexing recalls chunkable runtime memory",
		},
	})
	if err != nil {
		t.Fatalf("ImportDocument error = %v", err)
	}
	if len(result.Chunks) != 1 || result.Chunks[0].Scope.DatasetID != "dataset-1" || result.Chunks[0].DocumentID != "doc-1" {
		t.Fatalf("ImportDocument chunks = %+v, want stored dataset-1/doc-1 chunk", result.Chunks)
	}
	if chunker.calls != 1 {
		t.Fatalf("chunker calls = %d, want 1", chunker.calls)
	}
	stored, ok, err := mem.DocumentStore().Get(ctx, "dataset-1", "doc-1")
	if err != nil || !ok {
		t.Fatalf("DocumentStore.Get ok=%v err=%v, want stored canonical document", ok, err)
	}
	if stored.DatasetID != "dataset-1" || stored.ID != "doc-1" || stored.Version == 0 || stored.ContentHash == "" {
		t.Fatalf("stored document = %+v, want canonical fields assigned", stored)
	}
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "doc_chunks")

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: "chunkable runtime memory",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("PackContext document retrieval error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || pack.DocumentHits[0].Chunk.DocumentID != "doc-1" {
		t.Fatalf("DocumentHits = %+v, want imported document chunk", pack.DocumentHits)
	}
	if len(pack.Items) != 1 || pack.Items[0].Kind != memory.ContextItemDocumentChunk {
		t.Fatalf("PackContext Items = %+v, want document chunk item only", pack.Items)
	}
}

func TestMemoryFacadeImportDocumentScopesDocumentProjectionByUser(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New scoped document memory error = %v", err)
	}

	scopeOne := testScope("conv-doc")
	scopeOne.DatasetID = "dataset-1"
	scopeTwo := scopeOne
	scopeTwo.UserID = "user-2"
	for _, entry := range []struct {
		scope memory.Scope
		id    string
	}{
		{scope: scopeOne, id: "doc-user-1"},
		{scope: scopeTwo, id: "doc-user-2"},
	} {
		_, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
			Scope: entry.scope,
			Document: sourcedocument.Document{
				ID:      entry.id,
				Content: "same document text about scoped projection recall",
			},
		})
		if err != nil {
			t.Fatalf("ImportDocument scope %+v error = %v", entry.scope, err)
		}
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeOne,
		Query: "scoped projection recall",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext scoped document retrieval error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || pack.DocumentHits[0].Chunk.DocumentID != "doc-user-1" {
		t.Fatalf("DocumentHits = %+v, want only user-1 document", pack.DocumentHits)
	}
}

func TestMemoryFacadePackContextFiltersDocumentsByDataset(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New dataset document memory error = %v", err)
	}

	scopeA := testScope("conv-doc")
	scopeA.DatasetID = "dataset-a"
	scopeB := scopeA
	scopeB.DatasetID = "dataset-b"
	for _, entry := range []struct {
		scope memory.Scope
		id    string
	}{
		{scope: scopeA, id: "doc-a"},
		{scope: scopeB, id: "doc-b"},
	} {
		_, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
			Scope: entry.scope,
			Document: sourcedocument.Document{
				ID:      entry.id,
				Content: "shared dataset filter document recall",
			},
		})
		if err != nil {
			t.Fatalf("ImportDocument dataset %+v error = %v", entry.scope, err)
		}
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeA,
		Query: "dataset filter document recall",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext dataset document retrieval error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || pack.DocumentHits[0].Chunk.Scope.DatasetID != "dataset-a" {
		t.Fatalf("DocumentHits = %+v, want only dataset-a", pack.DocumentHits)
	}
}

func TestMemoryFacadeImportDocumentNormalizesScopeAndDocumentDataset(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New normalized document memory error = %v", err)
	}

	rawScope := testScope(" conv-doc ")
	rawScope.RuntimeID = " runtime-1 "
	rawScope.UserID = " user-1 "
	rawScope.DatasetID = " dataset-1 "
	result, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: rawScope,
		Document: sourcedocument.Document{
			DatasetID: " dataset-1 ",
			ID:        " doc-1 ",
			Content:   "trimmed dataset document recall",
		},
	})
	if err != nil {
		t.Fatalf("ImportDocument normalized scope error = %v", err)
	}
	wantScope := testScope("conv-doc")
	wantScope.DatasetID = "dataset-1"
	if len(result.Chunks) != 1 || result.Chunks[0].Scope != wantScope {
		t.Fatalf("ImportDocument chunks = %+v, want normalized scope %+v", result.Chunks, wantScope)
	}
	if _, ok, err := mem.DocumentStore().Get(ctx, "dataset-1", "doc-1"); err != nil || !ok {
		t.Fatalf("DocumentStore.Get normalized document ok = %v err %v, want true nil", ok, err)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: rawScope,
		Query: "trimmed dataset document recall",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext normalized document scope error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || pack.DocumentHits[0].Chunk.Scope != wantScope {
		t.Fatalf("DocumentHits = %+v, want normalized scope %+v", pack.DocumentHits, wantScope)
	}
}

func TestMemoryFacadeImportDocumentValidationAndDependencyErrors(t *testing.T) {
	ctx := context.Background()
	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"

	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New document memory error = %v", err)
	}
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			DatasetID: "other-dataset",
			ID:        "doc-1",
			Content:   "conflicting dataset",
		},
	}); err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "does not match scope dataset_id") {
		t.Fatalf("ImportDocument dataset conflict err = %v, want validation conflict", err)
	}

	missingStoreDeps := newDocumentDeps(t)
	missingStoreDeps.DocumentStore = nil
	missingStoreDeps.DocumentChunker = &fakeDocumentChunker{}
	missingStore, err := memory.New(documentStoreOnlySpec(), missingStoreDeps)
	if err != nil {
		t.Fatalf("New missing document store memory error = %v", err)
	}
	if _, err := missingStore.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-1", Content: "missing store"},
	}); err == nil || !errdefs.IsNotAvailable(err) || !strings.Contains(err.Error(), "document store is not configured") {
		t.Fatalf("ImportDocument missing store err = %v, want NotAvailable", err)
	}

	missingChunkDeps := newDocumentDeps(t)
	missingChunkDeps.ChunkStore = nil
	missingChunkDeps.DocumentChunker = nil
	missingChunks, err := memory.New(documentChunkStageSpec(), missingChunkDeps)
	if err != nil {
		t.Fatalf("New missing chunk flow memory error = %v", err)
	}
	if _, err := missingChunks.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-1", Content: "missing chunk flow"},
	}); err == nil || !errdefs.IsNotAvailable(err) || !strings.Contains(err.Error(), "chunk_document") {
		t.Fatalf("ImportDocument missing chunk flow err = %v, want chunk_document NotAvailable", err)
	}

	missingIndexDeps := newDocumentDeps(t)
	missingIndexDeps.Index = nil
	missingIndexDeps.DocumentChunker = &fakeDocumentChunker{}
	if _, err := memory.New(documentRetrievalSpec(), missingIndexDeps); err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "projections require Index") {
		t.Fatalf("New missing index err = %v, want projection validation error", err)
	}
}

func TestMemoryFacadePackContextFiltersSemanticRetrievalByScope(t *testing.T) {
	ctx := context.Background()
	spec := semanticRetrievalSpec()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New scoped semantic memory error = %v", err)
	}

	scopeOne := testScope("conv-1")
	scopeTwo := scopeOne
	scopeTwo.UserID = "user-2"
	for _, scope := range []memory.Scope{scopeOne, scopeTwo} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    scope,
			Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
		}); err != nil {
			t.Fatalf("AppendMessage scope %+v error = %v", scope, err)
		}
	}
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "observations")
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "facts")
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "fact_graph")

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeOne,
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext scoped semantic retrieval error = %v", err)
	}
	if len(pack.ObservationHits) != 1 || pack.ObservationHits[0].Observation.Scope != scopeOne {
		t.Fatalf("ObservationHits = %+v, want only scope one", pack.ObservationHits)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.Scope != scopeOne {
		t.Fatalf("FactHits = %+v, want only scope one", pack.FactHits)
	}
	for _, hit := range pack.FactGraphHits {
		if hit.Node != nil && hit.Node.Scope != scopeOne {
			t.Fatalf("FactGraph node hit scope = %+v, want %+v", hit.Node.Scope, scopeOne)
		}
		if hit.Edge != nil && hit.Edge.Scope != scopeOne {
			t.Fatalf("FactGraph edge hit scope = %+v, want %+v", hit.Edge.Scope, scopeOne)
		}
	}
}

func TestMemoryFacadeSemanticRetrievalSeparatesGlobalAndUserPartitions(t *testing.T) {
	ctx := context.Background()
	spec := semanticRetrievalSpec()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New global partition memory error = %v", err)
	}

	userScope := testScope("conv-global")
	globalScope := userScope
	globalScope.UserID = ""
	for _, scope := range []memory.Scope{userScope, globalScope} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    scope,
			Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
		}); err != nil {
			t.Fatalf("AppendMessage scope %+v error = %v", scope, err)
		}
	}

	userPack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: userScope,
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext user partition error = %v", err)
	}
	if len(userPack.ObservationHits) != 1 || userPack.ObservationHits[0].Observation.Scope != userScope {
		t.Fatalf("user ObservationHits = %+v, want only user scope", userPack.ObservationHits)
	}
	if len(userPack.FactHits) != 1 || userPack.FactHits[0].Fact.Scope != userScope {
		t.Fatalf("user FactHits = %+v, want only user scope", userPack.FactHits)
	}

	globalPack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: globalScope,
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext global partition error = %v", err)
	}
	if len(globalPack.ObservationHits) != 1 || globalPack.ObservationHits[0].Observation.Scope != globalScope {
		t.Fatalf("global ObservationHits = %+v, want only global scope", globalPack.ObservationHits)
	}
	if len(globalPack.FactHits) != 1 || globalPack.FactHits[0].Fact.Scope != globalScope {
		t.Fatalf("global FactHits = %+v, want only global scope", globalPack.FactHits)
	}
}

func TestMemoryFacadePackContextAgentSoftIsolation(t *testing.T) {
	ctx := context.Background()
	spec := semanticRetrievalSpec()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New agent scoped memory error = %v", err)
	}

	shared := testScope("conv-1")
	agentA := shared
	agentA.AgentID = "agent-a"
	agentB := shared
	agentB.AgentID = "agent-b"
	for _, scope := range []memory.Scope{shared, agentA, agentB} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    scope,
			Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
		}); err != nil {
			t.Fatalf("AppendMessage scope %+v error = %v", scope, err)
		}
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: agentA,
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext agent scoped retrieval error = %v", err)
	}
	gotAgents := map[string]bool{}
	for _, hit := range pack.ObservationHits {
		gotAgents[hit.Observation.Scope.AgentID] = true
	}
	if len(pack.ObservationHits) != 2 || !gotAgents[""] || !gotAgents["agent-a"] || gotAgents["agent-b"] {
		t.Fatalf("Observation agent hits = %+v, want shared and agent-a only", pack.ObservationHits)
	}
	for _, hit := range pack.FactHits {
		if hit.Fact.Scope.AgentID == "agent-b" {
			t.Fatalf("FactHits included other agent: %+v", pack.FactHits)
		}
	}
}

func TestMemoryFacadeNormalizesScopeBeforeWritingAndReading(t *testing.T) {
	ctx := context.Background()
	spec := semanticRetrievalSpec()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New normalized scope memory error = %v", err)
	}

	rawScope := memory.Scope{
		RuntimeID:      " runtime-1 ",
		UserID:         " user-1 ",
		AgentID:        " agent-1 ",
		ConversationID: " conv-normalized ",
		DatasetID:      " dataset-1 ",
		EntityID:       " entity-1 ",
	}
	wantScope := memory.Scope{
		RuntimeID:      "runtime-1",
		UserID:         "user-1",
		AgentID:        "agent-1",
		ConversationID: "conv-normalized",
		DatasetID:      "dataset-1",
		EntityID:       "entity-1",
	}
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    rawScope,
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	}); err != nil {
		t.Fatalf("AppendMessage normalized scope error = %v", err)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: rawScope,
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext normalized scope error = %v", err)
	}
	if len(pack.ObservationHits) != 1 || pack.ObservationHits[0].Observation.Scope != wantScope {
		t.Fatalf("ObservationHits = %+v, want normalized scope %+v", pack.ObservationHits, wantScope)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.Scope != wantScope {
		t.Fatalf("FactHits = %+v, want normalized scope %+v", pack.FactHits, wantScope)
	}
}

func TestMemoryFacadePackContextFiltersSummaryByConversation(t *testing.T) {
	ctx := context.Background()
	spec := memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summaries", Required: true}},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "build_summary_dag"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}
	deps := newDeps(t)
	deps.SummaryStore = recent.NewSummaryWorkspaceStore(sdkworkspace.NewMemWorkspace())
	deps.Summarizer = &fakeSummarizer{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New summary memory error = %v", err)
	}
	for _, conversationID := range []string{"conv-1", "conv-2"} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    testScope(conversationID),
			Messages: []sourcemessage.Message{messageWithText("summary should mention runtime memory")},
		}); err != nil {
			t.Fatalf("AppendMessage %s error = %v", conversationID, err)
		}
	}
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "summaries")

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: testScope("conv-1"),
		Query: "runtime summary",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext summary scoped retrieval error = %v", err)
	}
	if len(pack.SummaryHits) != 1 || pack.SummaryHits[0].Node.Scope.ConversationID != "conv-1" {
		t.Fatalf("SummaryHits = %+v, want only conv-1", pack.SummaryHits)
	}
}

func TestMemoryFacadeSummaryDAGUsesScopedPartitions(t *testing.T) {
	ctx := context.Background()
	spec := memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilitySummaryDAG, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilitySummaryDAG, Namespace: "summaries", Required: true}},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "build_summary_dag"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_summaries"},
			{Name: "pack_context"},
		},
	}
	deps := newDeps(t)
	deps.SummaryStore = recent.NewSummaryWorkspaceStore(sdkworkspace.NewMemWorkspace())
	deps.Summarizer = &fakeSummarizer{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New summary scoped memory error = %v", err)
	}

	scopeOne := testScope("shared-conv")
	scopeTwo := scopeOne
	scopeTwo.UserID = "user-2"
	for _, scope := range []memory.Scope{scopeOne, scopeTwo} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    scope,
			Messages: []sourcemessage.Message{messageWithText("summary should mention runtime memory")},
		}); err != nil {
			t.Fatalf("AppendMessage scope %+v error = %v", scope, err)
		}
	}
	assertBaseNamespaceEmpty(t, ctx, mem.RetrievalIndex(), "summaries")

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeOne,
		Query: "runtime summary",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext summary partition error = %v", err)
	}
	if len(pack.SummaryHits) != 1 || pack.SummaryHits[0].Node.Scope != scopeOne {
		t.Fatalf("SummaryHits = %+v, want only scope one", pack.SummaryHits)
	}
}

func TestMemoryFacadeAsyncWriteStagesDrainIntoReadPath(t *testing.T) {
	ctx := context.Background()
	spec := memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
			{Capability: memory.CapabilityFactLedger, Required: true},
			{Capability: memory.CapabilityFactGraph, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: memory.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: memory.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "extract_observations", Async: true},
			{Name: "reconcile_facts", Async: true},
			{Name: "build_fact_graph", Async: true},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_observations"},
			{Name: "retrieve_facts"},
			{Name: "retrieve_fact_graph"},
			{Name: "pack_context"},
		},
	}
	extractor := &fakeObservationExtractor{}
	deps := newDeps(t)
	deps.ObservationExtractor = extractor
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	scheduler := newRecordingScheduler()
	deps.Scheduler = scheduler
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New async memory error = %v", err)
	}

	scope := testScope("conv-1")
	scope.AgentID = "agent-1"
	scope.DatasetID = "dataset-1"
	scope.EntityID = "entity-1"
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    scope,
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage async error = %v", err)
	}
	if len(result.Jobs) != 1 || result.Jobs[0].ID == "" {
		t.Fatalf("AppendMessage Jobs = %+v, want one queued job", result.Jobs)
	}
	if len(scheduler.jobs) != 1 || scheduler.jobs[0].Scope != scope {
		t.Fatalf("queued job scope = %+v, want full scope %+v", scheduler.jobs, scope)
	}
	if len(result.Observations) != 0 || len(result.Facts) != 0 || result.FactGraph != nil {
		t.Fatalf("AppendMessage async sync outputs = %+v, want none before scheduler runs", result)
	}
	stats, err := mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats before drain error = %v", err)
	}
	if stats.Pending != 1 || stats.Completed != 0 {
		t.Fatalf("QueueStats before drain = %+v, want one pending", stats)
	}

	before, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope, Query: "Ada tea likes"})
	if err != nil {
		t.Fatalf("PackContext before drain error = %v", err)
	}
	if len(before.ObservationHits) != 0 || len(before.FactHits) != 0 || len(before.FactGraphHits) != 0 {
		t.Fatalf("PackContext before drain hits = obs:%d facts:%d graph:%d, want no derived hits", len(before.ObservationHits), len(before.FactHits), len(before.FactGraphHits))
	}

	if err := mem.Drain(ctx); err != nil {
		t.Fatalf("Drain error = %v", err)
	}
	stats, err = mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats after drain error = %v", err)
	}
	if stats.Pending != 0 || stats.Completed != 1 || stats.Failed != 0 {
		t.Fatalf("QueueStats after drain = %+v, want one completed", stats)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls after async drain = %d, want 1", extractor.calls)
	}
	after, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope, Query: "Ada tea likes"})
	if err != nil {
		t.Fatalf("PackContext after drain error = %v", err)
	}
	if len(after.ObservationHits) != 1 || len(after.FactHits) != 1 || len(after.FactGraphHits) == 0 {
		t.Fatalf("PackContext after drain hits = obs:%d facts:%d graph:%d, want derived hits", len(after.ObservationHits), len(after.FactHits), len(after.FactGraphHits))
	}
}

func TestMemoryFacadeAsyncWriteStagesRequireScheduler(t *testing.T) {
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	_, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		WriteStages: []memory.StageSpec{{Name: "extract_observations", Async: true}},
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "async write stages require Scheduler") {
		t.Fatalf("New async without scheduler err = %v, want scheduler validation error", err)
	}
}

func TestMemoryFacadeRunOnceDrainAndShutdownScheduler(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.Scheduler = memory.NewMemoryScheduler()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		WriteStages: []memory.StageSpec{{Name: "extract_observations", Async: true}},
	}, deps)
	if err != nil {
		t.Fatalf("New scheduler memory error = %v", err)
	}
	for _, conversationID := range []string{"conv-1", "conv-2"} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    testScope(conversationID),
			Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
		}); err != nil {
			t.Fatalf("AppendMessage %s error = %v", conversationID, err)
		}
	}
	stats, err := mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats pending error = %v", err)
	}
	if stats.Pending != 2 {
		t.Fatalf("QueueStats pending = %+v, want two pending", stats)
	}
	result, err := mem.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if !result.Completed || result.JobID == "" || result.Error != "" {
		t.Fatalf("RunOnce result = %+v, want completed job", result)
	}
	stats, err = mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats after RunOnce error = %v", err)
	}
	if stats.Pending != 1 || stats.Completed != 1 {
		t.Fatalf("QueueStats after RunOnce = %+v, want one pending and one completed", stats)
	}
	if err := mem.Drain(ctx); err != nil {
		t.Fatalf("Drain remaining jobs error = %v", err)
	}
	if err := mem.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
}

func TestMemoryFacadeUnknownRequiredStageErrors(t *testing.T) {
	deps := newDeps(t)
	_, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		WriteStages:  []memory.StageSpec{{Name: "invent_memories"}},
	}, deps)
	if err == nil || !strings.Contains(err.Error(), `unsupported write stage "invent_memories"`) {
		t.Fatalf("New unknown write stage err = %v, want unsupported stage error", err)
	}

	_, err = memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		ReadStages:   []memory.StageSpec{{Name: "retrieve_moon_phase"}},
	}, deps)
	if err == nil || !strings.Contains(err.Error(), `unsupported read stage "retrieve_moon_phase"`) {
		t.Fatalf("New unknown read stage err = %v, want unsupported stage error", err)
	}

	_, err = memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Lifecycle:    []memory.StageSpec{{Name: "compact"}},
	}, deps)
	if err == nil || !strings.Contains(err.Error(), `unsupported lifecycle stage "compact"`) {
		t.Fatalf("New required unsupported lifecycle stage err = %v, want unsupported lifecycle error", err)
	}
}

func TestMemoryFacadeOptionalUnknownStageSkipped(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		WriteStages:  []memory.StageSpec{{Name: "future_write_stage", Optional: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "future_read_stage", Optional: true},
			{Name: "pack_context"},
		},
		Lifecycle: []memory.StageSpec{
			{Name: "readiness"},
			{Name: "compact", Optional: true},
			{Name: "future_lifecycle_stage", Optional: true},
			{Name: "shutdown"},
		},
	}, deps)
	if err != nil {
		t.Fatalf("New optional unknown stages error = %v", err)
	}
	if got, want := len(mem.Plan().Lifecycle), 2; got != want {
		t.Fatalf("Plan().Lifecycle len = %d, want optional unsupported stages skipped to %d", got, want)
	}
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    testScope("conv-1"),
		Messages: []sourcemessage.Message{messageWithText("window only")},
	})
	if err != nil {
		t.Fatalf("AppendMessage with optional unknown stage error = %v", err)
	}
	if len(result.Observations) != 0 || len(result.Facts) != 0 || result.FactGraph != nil {
		t.Fatalf("AppendMessage optional unknown result = %+v, want no derived records", result)
	}
	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: testScope("conv-1")})
	if err != nil {
		t.Fatalf("PackContext with optional unknown stage error = %v", err)
	}
	if len(pack.Items) != 1 || pack.Items[0].Kind != memory.ContextItemRecentMessage {
		t.Fatalf("PackContext optional unknown Items = %+v, want recent message only", pack.Items)
	}
}

func TestMemoryFacadeReadStageCannotBeAsync(t *testing.T) {
	deps := newDeps(t)
	_, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		ReadStages:   []memory.StageSpec{{Name: "load_recent_messages", Async: true}},
	}, deps)
	if err == nil || !strings.Contains(err.Error(), `read stage "load_recent_messages" cannot be async`) {
		t.Fatalf("New async read stage err = %v, want validation error", err)
	}
}

func TestMemoryFacadeRetrievalReadStageRequiresQuery(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		Projections: []memory.ProjectionSpec{{Capability: memory.CapabilityObservationLedger, Namespace: "observations", Required: true}},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_observations"},
			{Name: "pack_context"},
		},
	}, deps)
	if err != nil {
		t.Fatalf("New query validation memory error = %v", err)
	}
	if _, err := mem.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: "conv-1",
		Messages:       []sourcemessage.Message{messageWithText("Ada likes tea.")},
	}); err != nil {
		t.Fatalf("Append setup message error = %v", err)
	}
	_, err = mem.PackContext(ctx, memory.ContextRequest{Scope: testScope("conv-1")})
	if err == nil || !strings.Contains(err.Error(), `read stage "retrieve_observations" requires query`) {
		t.Fatalf("PackContext empty query err = %v, want query validation error", err)
	}
}

func TestMemoryFacadeContextRequestRejectsConflictingWindowScope(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, deps)
	if err != nil {
		t.Fatalf("New window scope validation memory error = %v", err)
	}

	scope := testScope("conv-1")
	scope.AgentID = "agent-1"
	scope.DatasetID = "dataset-1"
	scope.EntityID = "entity-1"
	if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    scope,
		Messages: []sourcemessage.Message{messageWithText("window scope validation")},
	}); err != nil {
		t.Fatalf("AppendMessage setup error = %v", err)
	}

	matchingWindowScope := memory.Scope{ConversationID: " conv-1 ", UserID: " user-1 "}
	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Window: recent.WindowRequest{
			Scope:  matchingWindowScope,
			Budget: recent.WindowBudget{MaxMessages: 1},
		},
	})
	if err != nil {
		t.Fatalf("PackContext matching nested window scope error = %v", err)
	}
	if len(pack.Window.Messages) != 1 {
		t.Fatalf("PackContext matching nested window scope messages = %d, want 1", len(pack.Window.Messages))
	}

	tests := []struct {
		name  string
		scope memory.Scope
		want  string
	}{
		{name: "runtime", scope: memory.Scope{RuntimeID: "runtime-2"}, want: "window runtime_id"},
		{name: "user", scope: memory.Scope{UserID: "user-2"}, want: "window user_id"},
		{name: "agent", scope: memory.Scope{AgentID: "agent-2"}, want: "window agent_id"},
		{name: "conversation", scope: memory.Scope{ConversationID: "conv-2"}, want: "window conversation_id"},
		{name: "dataset", scope: memory.Scope{DatasetID: "dataset-2"}, want: "window dataset_id"},
		{name: "entity", scope: memory.Scope{EntityID: "entity-2"}, want: "window entity_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mem.PackContext(ctx, memory.ContextRequest{
				Scope:  scope,
				Window: recent.WindowRequest{Scope: tc.scope},
			})
			if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PackContext conflicting window scope err = %v, want validation containing %q", err, tc.want)
			}
		})
	}
}

func TestMemoryFacadeReadinessReportsMissingDependencies(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationStore = nil
	deps.ObservationExtractor = nil
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityObservationLedger},
		},
	}, deps)
	if err != nil {
		t.Fatalf("New readiness memory error = %v", err)
	}
	report, err := mem.Readiness(ctx)
	if err != nil {
		t.Fatalf("Readiness error = %v", err)
	}
	if report.Ready {
		t.Fatalf("Readiness Ready = true, want false for missing optional capability deps: %+v", report)
	}
	assertReadinessCheck(t, report, "capability.observation_ledger.store", false)
	assertReadinessCheck(t, report, "capability.observation_ledger.service", false)
}

func TestMemoryFacadeRebuildAndReconcileAreExplicitlyUnavailable(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New lifecycle unavailable memory error = %v", err)
	}
	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{Scope: testScope("conv-1")})
	if err == nil || !errdefs.IsNotAvailable(err) || rebuild.Supported {
		t.Fatalf("Rebuild result=%+v err=%v, want unsupported NotAvailable", rebuild, err)
	}
	reconcile, err := mem.Reconcile(ctx, memory.ReconcileRequest{Scope: testScope("conv-1")})
	if err == nil || !errdefs.IsNotAvailable(err) || reconcile.Supported {
		t.Fatalf("Reconcile result=%+v err=%v, want unsupported NotAvailable", reconcile, err)
	}
}

func TestNoPublicRecipeHelpers(t *testing.T) {
	files := parseCurrentMemoryPackage(t)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			name := fn.Name.Name
			if name == "LongAssistantRecipe" || name == "KnowledgeHeavyRecipe" || strings.HasSuffix(name, "Recipe") {
				t.Fatalf("unexpected public recipe helper %s", name)
			}
		}
	}
}

func TestSystemDoesNotExposeVerticalExecutorMethods(t *testing.T) {
	forbidden := map[string]bool{
		"IndexDocument":        true,
		"SearchDocumentChunks": true,
		"BuildSummaryDAG":      true,
		"SearchSummaryNodes":   true,
		"ExtractObservations":  true,
		"SearchObservations":   true,
		"ReconcileFacts":       true,
		"SearchFacts":          true,
		"BuildFactGraph":       true,
		"SearchFactGraph":      true,
		"PackContextRaw":       true,
	}

	files := parseCurrentMemoryPackage(t)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isSystemReceiver(fn) {
				continue
			}
			if forbidden[fn.Name.Name] {
				t.Fatalf("System exposes low-level executor method %s", fn.Name.Name)
			}
		}
	}
}

func TestPublicExecutorTypeAliasesAreCentralized(t *testing.T) {
	const executorImportPath = "github.com/GizClaw/flowcraft/memory/internal/executor"

	expectedTypes := map[string]string{
		"DocumentChunkSearchResponse": "DocumentChunkSearchResponse",
		"DocumentChunkSearchHit":      "DocumentChunkSearchHit",
		"SummaryNodeSearchResponse":   "SummaryNodeSearchResponse",
		"SummaryNodeSearchHit":        "SummaryNodeSearchHit",
		"ObservationSearchResponse":   "ObservationSearchResponse",
		"ObservationSearchHit":        "ObservationSearchHit",
		"FactSearchResponse":          "FactSearchResponse",
		"FactSearchHit":               "FactSearchHit",
		"FactGraphBuildResult":        "FactGraphBuildResult",
		"FactGraphSearchResponse":     "FactGraphSearchResponse",
		"FactGraphSearchHit":          "FactGraphSearchHit",
		"ContextPack":                 "ContextPack",
		"ContextItemKind":             "ContextItemKind",
		"ContextItem":                 "ContextItem",
	}
	expectedConstants := map[string]string{
		"ContextItemRecentMessage": "ContextItemRecentMessage",
		"ContextItemSummaryNode":   "ContextItemSummaryNode",
		"ContextItemDocumentChunk": "ContextItemDocumentChunk",
		"ContextItemObservation":   "ContextItemObservation",
		"ContextItemFact":          "ContextItemFact",
		"ContextItemFactGraphNode": "ContextItemFactGraphNode",
		"ContextItemFactGraphEdge": "ContextItemFactGraphEdge",
	}
	seenTypes := map[string]bool{}
	seenConstants := map[string]bool{}

	files := parseCurrentMemoryPackage(t)
	for filename, file := range files {
		executorImports := map[string]bool{}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import path %s: %v", imp.Path.Value, err)
			}
			if path != executorImportPath {
				continue
			}
			name := "executor"
			if imp.Name != nil {
				name = imp.Name.Name
			}
			executorImports[name] = true
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.TYPE:
				for _, spec := range gen.Specs {
					typeSpec := spec.(*ast.TypeSpec)
					if !typeSpec.Name.IsExported() || !typeSpec.Assign.IsValid() {
						continue
					}
					name := typeSpec.Name.Name
					selector, ok := typeSpec.Type.(*ast.SelectorExpr)
					if !ok || !isExecutorSelector(selector, executorImports) {
						continue
					}
					wantSelector, ok := expectedTypes[name]
					if !ok {
						t.Fatalf("unexpected public executor alias %s in %s", name, filename)
					}
					if !strings.HasSuffix(filename, "types.go") {
						t.Fatalf("public executor alias %s must be centralized in types.go, found in %s", name, filename)
					}
					ident := selector.X.(*ast.Ident)
					if ident.Name != "internalexecutor" {
						t.Fatalf("public executor alias %s uses %s.%s, want internalexecutor.%s", name, ident.Name, selector.Sel.Name, wantSelector)
					}
					if selector.Sel.Name != wantSelector {
						t.Fatalf("public executor alias %s = %s, want %s", name, selector.Sel.Name, wantSelector)
					}
					seenTypes[name] = true
				}
			case token.CONST:
				for _, spec := range gen.Specs {
					valueSpec := spec.(*ast.ValueSpec)
					for i, nameIdent := range valueSpec.Names {
						if !nameIdent.IsExported() || i >= len(valueSpec.Values) {
							continue
						}
						selector, ok := valueSpec.Values[i].(*ast.SelectorExpr)
						if !ok || !isExecutorSelector(selector, executorImports) {
							continue
						}
						name := nameIdent.Name
						wantSelector, ok := expectedConstants[name]
						if !ok {
							t.Fatalf("unexpected public executor constant alias %s in %s", name, filename)
						}
						if !strings.HasSuffix(filename, "types.go") {
							t.Fatalf("public executor constant alias %s must be centralized in types.go, found in %s", name, filename)
						}
						ident := selector.X.(*ast.Ident)
						if ident.Name != "internalexecutor" {
							t.Fatalf("public executor constant alias %s uses %s.%s, want internalexecutor.%s", name, ident.Name, selector.Sel.Name, wantSelector)
						}
						if selector.Sel.Name != wantSelector {
							t.Fatalf("public executor constant alias %s = %s, want %s", name, selector.Sel.Name, wantSelector)
						}
						seenConstants[name] = true
					}
				}
			}
		}
	}
	for name := range expectedTypes {
		if !seenTypes[name] {
			t.Fatalf("missing centralized public executor alias %s", name)
		}
	}
	for name := range expectedConstants {
		if !seenConstants[name] {
			t.Fatalf("missing centralized public executor constant alias %s", name)
		}
	}
}

func TestPublicRequestTypesDoNotExposeNamespaceOverrides(t *testing.T) {
	files := parseCurrentMemoryPackage(t)

	for filename, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				name := typeSpec.Name.Name
				if name == "PackContextRequest" {
					t.Fatalf("public PackContextRequest exposed in %s; use ContextRequest facade instead", filename)
				}
				if !typeSpec.Name.IsExported() || !strings.HasSuffix(name, "Request") {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structType.Fields.List {
					for _, fieldName := range field.Names {
						if strings.Contains(fieldName.Name, "Namespace") {
							t.Fatalf("public request %s exposes namespace override field %s in %s", name, fieldName.Name, filename)
						}
					}
				}
			}
		}
	}
}

func parseCurrentMemoryPackage(t *testing.T) map[string]*ast.File {
	t.Helper()

	fs := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir error = %v", err)
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		filename := filepath.Join(".", name)
		file, err := parser.ParseFile(fs, filename, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile %s error = %v", filename, err)
		}
		if file.Name.Name != "memory" {
			continue
		}
		files[filename] = file
	}
	if len(files) == 0 {
		t.Fatal("package memory not found")
	}
	return files
}

func isExecutorSelector(selector *ast.SelectorExpr, executorImports map[string]bool) bool {
	ident, ok := selector.X.(*ast.Ident)
	return ok && executorImports[ident.Name]
}

func isSystemReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	expr := fn.Recv.List[0].Type
	if ptr, ok := expr.(*ast.StarExpr); ok {
		expr = ptr.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "System"
}

type recordingScheduler struct {
	inner *memory.MemoryScheduler
	jobs  []memory.Job
}

func newRecordingScheduler() *recordingScheduler {
	return &recordingScheduler{inner: memory.NewMemoryScheduler()}
}

func (s *recordingScheduler) Enqueue(ctx context.Context, job memory.Job) (memory.JobHandle, error) {
	s.jobs = append(s.jobs, job)
	return s.inner.Enqueue(ctx, job)
}

func (s *recordingScheduler) RunOnce(ctx context.Context) (memory.JobResult, error) {
	return s.inner.RunOnce(ctx)
}

func (s *recordingScheduler) Drain(ctx context.Context) error {
	return s.inner.Drain(ctx)
}

func (s *recordingScheduler) Shutdown(ctx context.Context) error {
	return s.inner.Shutdown(ctx)
}

func (s *recordingScheduler) Stats(ctx context.Context) (memory.QueueStats, error) {
	return s.inner.Stats(ctx)
}

func newDeps(t *testing.T) memory.Deps {
	t.Helper()
	ws := sdkworkspace.NewMemWorkspace()
	index, err := retrievalworkspace.New(sdkworkspace.Sub(ws, "retrieval"))
	if err != nil {
		t.Fatalf("create retrieval index error = %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Fatalf("close retrieval index error = %v", err)
		}
	})
	return memory.Deps{
		MessageStore:     sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/message")),
		ObservationStore: viewobservation.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/observation_ledger")),
		FactStore:        fact.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_ledger")),
		FactGraphStore:   fact.NewGraphWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_graph")),
		Index:            index,
	}
}

func newDocumentDeps(t *testing.T) memory.Deps {
	t.Helper()
	deps := newDeps(t)
	ws := sdkworkspace.NewMemWorkspace()
	deps.DocumentStore = sourcedocument.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/document"))
	deps.ChunkStore = viewdocument.NewChunkWorkspaceStore(sdkworkspace.Sub(ws, "views/document_chunks"))
	return deps
}

func assertNamespace(t *testing.T, rt *memory.System, capability memory.Capability, want string) {
	t.Helper()
	got, ok := rt.ProjectionNamespace(capability)
	if !ok {
		t.Fatalf("ProjectionNamespace(%q) ok = false", capability)
	}
	if got != want {
		t.Fatalf("ProjectionNamespace(%q) = %q, want %q", capability, got, want)
	}
}

func assertBaseNamespaceEmpty(t *testing.T, ctx context.Context, index retrieval.Index, namespace string) {
	t.Helper()
	resp, err := index.Search(ctx, namespace, retrieval.SearchRequest{QueryText: "Ada tea likes", TopK: 10})
	if err != nil {
		t.Fatalf("Search base namespace %q error = %v", namespace, err)
	}
	if resp != nil && len(resp.Hits) != 0 {
		t.Fatalf("Search base namespace %q hits = %+v, want none", namespace, resp.Hits)
	}
}

func assertReadinessCheck(t *testing.T, report memory.ReadinessReport, name string, ready bool) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			if check.Ready != ready {
				t.Fatalf("Readiness check %q Ready = %v, want %v; report=%+v", name, check.Ready, ready, report)
			}
			return
		}
	}
	t.Fatalf("Readiness check %q missing from report %+v", name, report)
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

func testScope(conversationID string) memory.Scope {
	return memory.Scope{
		RuntimeID:      "runtime-1",
		UserID:         "user-1",
		ConversationID: conversationID,
	}
}

func documentRetrievalSpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{
			{Kind: memory.SourceMessageLog, Required: true},
			{Kind: memory.SourceDocumentStore, Required: true},
		},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityDocumentChunks, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityDocumentChunks, Namespace: "doc_chunks", Required: true},
		},
	}
}

func documentStoreOnlySpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{
			{Kind: memory.SourceDocumentStore},
		},
	}
}

func documentChunkStageSpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{
			{Kind: memory.SourceDocumentStore},
		},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityDocumentChunks},
		},
		WriteStages: []memory.StageSpec{
			{Name: "chunk_document"},
		},
	}
}

func semanticRetrievalSpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
			{Capability: memory.CapabilityFactLedger, Required: true},
			{Capability: memory.CapabilityFactGraph, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: memory.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: memory.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "extract_observations"},
			{Name: "reconcile_facts"},
			{Name: "build_fact_graph"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_observations"},
			{Name: "retrieve_facts"},
			{Name: "retrieve_fact_graph"},
			{Name: "pack_context"},
		},
	}
}

type fakeObservationExtractor struct {
	calls int
}

type fakeDocumentChunker struct {
	calls int
}

type fakeSummarizer struct{}

func (f *fakeDocumentChunker) ChunkDocument(_ context.Context, input memory.DocumentChunkInput) ([]viewdocument.Chunk, error) {
	f.calls++
	if strings.TrimSpace(input.Document.Content) == "" {
		return nil, nil
	}
	doc := input.Document
	span := views.Span{Start: 0, End: len(doc.Content)}
	ref := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:   doc.DatasetID,
			DocumentID:  doc.ID,
			Version:     strconv.FormatUint(doc.Version, 10),
			ContentHash: doc.ContentHash,
			Span:        &span,
		},
	}
	return []viewdocument.Chunk{{
		ID:         "whole",
		Scope:      input.Scope,
		DocumentID: doc.ID,
		Layer: viewdocument.Layer{
			Name:               "whole_document",
			Version:            "v1",
			TransformSignature: "test-whole-document:v1",
		},
		Ordinal:   0,
		Span:      span,
		Text:      doc.Content,
		SourceRef: ref,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			SourceRevisions: []views.SourceRevision{{
				Kind:        views.SourceDocument,
				SourceKey:   ref.StableKey(),
				Revision:    strconv.FormatUint(doc.Version, 10),
				ContentHash: doc.ContentHash,
				ObservedAt:  doc.UpdatedAt,
			}},
			TransformSignature: "test-whole-document:v1",
		},
	}}, nil
}

func (f *fakeSummarizer) Summarize(_ context.Context, input memory.SummaryInput) ([]recent.SummaryNode, error) {
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

func (f *fakeObservationExtractor) ExtractObservations(_ context.Context, input memory.ObservationInput) ([]viewobservation.Observation, error) {
	f.calls++
	if len(input.Window.Messages) == 0 {
		return nil, nil
	}
	sourceRefs := input.Window.SourceRefs
	return []viewobservation.Observation{{
		ID:         scopedID("obs", input.Scope),
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

func (f *fakeFactReconciler) ReconcileFacts(_ context.Context, input memory.FactReconcileInput) ([]fact.Fact, error) {
	f.calls++
	if len(input.Observations) == 0 {
		return nil, nil
	}
	obs := input.Observations[0]
	return []fact.Fact{{
		ID:         fact.FactID(scopedID("fact", obs.Scope)),
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

func (f *fakeFactGraphBuilder) BuildFactGraph(_ context.Context, input memory.FactGraphInput) (memory.FactGraphOutput, error) {
	f.calls++
	if len(input.Facts) == 0 {
		return memory.FactGraphOutput{}, nil
	}
	record := input.Facts[0]
	sourceRefs := record.SourceRefs
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	subjectNodeID := fact.NodeID(scopedID("node-subject", record.Scope))
	objectNodeID := fact.NodeID(scopedID("node-object", record.Scope))
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
	return memory.FactGraphOutput{
		Nodes: []fact.Node{
			{
				ID:         subjectNodeID,
				Scope:      record.Scope,
				Kind:       fact.NodeEntity,
				Label:      "Ada",
				Aliases:    []string{record.Subject},
				FactRefs:   factRefs,
				SourceRefs: sourceRefs,
				Signature:  signature,
			},
			{
				ID:         objectNodeID,
				Scope:      record.Scope,
				Kind:       fact.NodeValue,
				Label:      record.Object,
				FactRefs:   factRefs,
				SourceRefs: sourceRefs,
				Signature:  signature,
			},
		},
		Edges: []fact.Edge{{
			ID:         fact.EdgeID(scopedID("edge", record.Scope)),
			Scope:      record.Scope,
			From:       subjectNodeID,
			To:         objectNodeID,
			Predicate:  record.Predicate,
			Status:     fact.FactActive,
			Confidence: record.Confidence,
			FactRefs:   factRefs,
			SourceRefs: sourceRefs,
			Signature:  signature,
		}},
	}, nil
}

func scopedID(prefix string, scope memory.Scope) string {
	if scope == testScope("conv-1") {
		switch prefix {
		case "obs":
			return "obs-1"
		case "fact":
			return "fact-1"
		case "node-subject":
			return "node-subject"
		case "node-object":
			return "node-object"
		case "edge":
			return "edge-1"
		}
	}
	parts := []string{prefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID}
	for i, part := range parts {
		if part == "" {
			parts[i] = "global"
			continue
		}
		parts[i] = strings.NewReplacer(":", "-", "/", "-", " ", "-").Replace(part)
	}
	return strings.Join(parts, "-")
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
