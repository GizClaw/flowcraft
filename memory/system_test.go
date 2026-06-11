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
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/internal/compiler"
	"github.com/GizClaw/flowcraft/memory/internal/projectors"
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

func TestMemoryFacadePackContextOnlyReturnsActiveFacts(t *testing.T) {
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
projections:
  - capability: fact_ledger
    namespace: facts
    required: true
write_stages:
  - name: append_message
  - name: extract_observations
  - name: reconcile_facts
read_stages:
  - name: load_recent_messages
  - name: retrieve_facts
  - name: pack_context
`
	spec, err := memory.Decode(strings.NewReader(specYAML))
	if err != nil {
		t.Fatalf("Decode lifecycle yaml spec error = %v", err)
	}
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = lifecycleFactReconciler{}

	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Fatalf("Close error = %v", err)
		}
	})

	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    testScope("conv-1"),
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage error = %v", err)
	}
	if len(result.Facts) != 4 {
		t.Fatalf("AppendMessage facts = %+v, want active plus three non-active ledger records", result.Facts)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: testScope("conv-1"),
		Query: "Ada likes tea",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext error = %v", err)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.ID != "fact-active" {
		t.Fatalf("PackContext FactHits = %+v, want only active fact", pack.FactHits)
	}
	var factItems int
	for _, item := range pack.Items {
		if item.Kind != derive.ContextItemFact {
			continue
		}
		factItems++
		if item.Fact == nil || item.Fact.ID != "fact-active" || item.Fact.Status != fact.FactActive {
			t.Fatalf("PackContext fact item = %+v, want only active fact item", item)
		}
	}
	if factItems != 1 {
		t.Fatalf("PackContext fact item count = %d, want 1 active fact item; items=%+v", factItems, pack.Items)
	}
}

func TestMemoryFacadeEntityProfileAndTimelinePackContext(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
	deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
	mem, err := memory.New(entityRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New entity memory error = %v", err)
	}

	scope := testScope("conv-entity")
	scope.EntityID = "user:ada"
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		TraceID:  "trace-async-write",
		Scope:    scope,
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage entity stages error = %v", err)
	}
	if len(result.EntityProfiles) != 1 || result.EntityProfiles[0].Scope != scope {
		t.Fatalf("AppendMessage EntityProfiles = %+v, want scoped profile", result.EntityProfiles)
	}
	if len(result.EntityEvents) != 1 || result.EntityEvents[0].Scope != scope {
		t.Fatalf("AppendMessage EntityEvents = %+v, want scoped event", result.EntityEvents)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: "Ada tea",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext entity stages error = %v", err)
	}
	if len(pack.EntityProfileHits) != 1 || pack.EntityProfileHits[0].Profile.ID != result.EntityProfiles[0].ID {
		t.Fatalf("EntityProfileHits = %+v, want hydrated profile", pack.EntityProfileHits)
	}
	if len(pack.EntityTimelineHits) != 1 || pack.EntityTimelineHits[0].Event.ID != result.EntityEvents[0].ID {
		t.Fatalf("EntityTimelineHits = %+v, want hydrated event", pack.EntityTimelineHits)
	}
	kinds := map[derive.ContextItemKind]bool{}
	for _, item := range pack.Items {
		kinds[item.Kind] = true
	}
	if !kinds[derive.ContextItemEntityProfile] || !kinds[derive.ContextItemEntityTimeline] {
		t.Fatalf("PackContext Items = %+v, want entity profile and timeline items", pack.Items)
	}
}

func TestMemoryFacadeEntityRetrievalScopeIsolation(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
	deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
	mem, err := memory.New(entityRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New scoped entity memory error = %v", err)
	}

	scopeOne := testScope("conv-entity")
	scopeOne.EntityID = "entity-1"
	scopeOtherRuntime := scopeOne
	scopeOtherRuntime.RuntimeID = "runtime-2"
	scopeOtherUser := scopeOne
	scopeOtherUser.UserID = "user-2"
	scopeOtherEntity := scopeOne
	scopeOtherEntity.EntityID = "entity-2"
	for _, scope := range []memory.Scope{scopeOne, scopeOtherRuntime, scopeOtherUser, scopeOtherEntity} {
		if _, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
			Scope:    scope,
			Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
		}); err != nil {
			t.Fatalf("AppendMessage scope %+v error = %v", scope, err)
		}
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scopeOne,
		Query: "Ada tea",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext scoped entity retrieval error = %v", err)
	}
	if len(pack.EntityProfileHits) != 1 || pack.EntityProfileHits[0].Profile.Scope != scopeOne {
		t.Fatalf("EntityProfileHits = %+v, want only scope one", pack.EntityProfileHits)
	}
	if len(pack.EntityTimelineHits) != 1 || pack.EntityTimelineHits[0].Event.Scope != scopeOne {
		t.Fatalf("EntityTimelineHits = %+v, want only scope one", pack.EntityTimelineHits)
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

func TestMemoryFacadeDefaultsEntityStagesWhenAvailable(t *testing.T) {
	spec := entityRetrievalSpec()
	spec.WriteStages = nil
	spec.ReadStages = nil
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	deps.EntityProfileBuilder = &fakeEntityProfileBuilder{}
	deps.EntityTimelineBuilder = &fakeEntityTimelineBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New default entity stage memory error = %v", err)
	}

	plan := mem.Plan()
	if !plannedStageNamed(plan.Write, "build_entity_profiles") || !plannedStageNamed(plan.Write, "build_entity_timeline") {
		t.Fatalf("Plan().Write = %+v, want default entity write stages", plan.Write)
	}
	if !plannedStageNamed(plan.Read, "retrieve_entity_profiles") || !plannedStageNamed(plan.Read, "retrieve_entity_timeline") {
		t.Fatalf("Plan().Read = %+v, want default entity read stages", plan.Read)
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
	if len(pack.Items) != 1 || pack.Items[0].Kind != derive.ContextItemDocumentChunk {
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

func TestMemoryFacadeContextPackerCanChangePackedItems(t *testing.T) {
	ctx := context.Background()
	scope := testScope("conv-1")
	packer := &fakeMemoryContextPacker{
		fn: func(input derive.ContextPackInput) (derive.ContextPackOutput, error) {
			if input.Scope != scope {
				t.Fatalf("ContextPacker Scope = %+v, want %+v", input.Scope, scope)
			}
			if input.Query != "Ada tea" {
				t.Fatalf("ContextPacker Query = %q, want Ada tea", input.Query)
			}
			if len(input.Window.Messages) != 1 || len(input.Items) != 1 {
				t.Fatalf("ContextPacker input window/items = %d/%d, want 1/1", len(input.Window.Messages), len(input.Items))
			}
			item := input.Items[0]
			item.Text = "hook-selected context"
			return derive.ContextPackOutput{Items: []derive.ContextItem{item}}, nil
		},
	}
	deps := newDeps(t)
	deps.ContextPacker = packer
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "pack_context"},
		},
	}, deps)
	if err != nil {
		t.Fatalf("New(context packer facade) error = %v", err)
	}
	if _, err := mem.MessageStore().Append(ctx, sourcemessage.AppendRequest{
		ConversationID: scope.ConversationID,
		Messages:       []sourcemessage.Message{messageWithText("Ada likes tea.")},
	}); err != nil {
		t.Fatalf("Append message error = %v", err)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope, Query: "Ada tea"})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if packer.calls != 1 {
		t.Fatalf("ContextPacker calls = %d, want 1", packer.calls)
	}
	if len(pack.Items) != 1 || pack.Items[0].Text != "hook-selected context" {
		t.Fatalf("PackContext Items = %+v, want hook-selected item", pack.Items)
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

func TestMemoryFacadePackContextReturnsOnlyActiveFacts(t *testing.T) {
	ctx := context.Background()
	spec := semanticRetrievalSpec()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.FactReconciler = &fakeFactReconciler{statuses: []fact.FactStatus{
		fact.FactActive,
		fact.FactRetracted,
		fact.FactSuperseded,
		fact.FactConflict,
	}}
	deps.FactGraphBuilder = &fakeFactGraphBuilder{}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New active-only semantic memory error = %v", err)
	}

	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		Scope:    testScope("conv-1"),
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage active-only facts error = %v", err)
	}
	if len(result.Facts) != 4 {
		t.Fatalf("AppendMessage facts = %+v, want active plus non-active ledger facts", result.Facts)
	}

	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: testScope("conv-1"),
		Query: "Ada tea likes",
		TopK:  10,
	})
	if err != nil {
		t.Fatalf("PackContext active-only facts error = %v", err)
	}
	if len(pack.FactHits) != 1 || pack.FactHits[0].Fact.Status != fact.FactActive {
		t.Fatalf("FactHits = %+v, want only active fact", pack.FactHits)
	}
	for _, item := range pack.Items {
		if item.Kind == derive.ContextItemFact && (item.Fact == nil || item.Fact.Status != fact.FactActive) {
			t.Fatalf("PackContext fact item = %+v, want only active fact items", item)
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
	jobStore := newRecordingJobStore()
	reportStore := memory.NewMemoryReportStore()
	deps.JobStore = jobStore
	deps.ReportStore = reportStore
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New async memory error = %v", err)
	}

	scope := testScope("conv-1")
	scope.AgentID = "agent-1"
	scope.DatasetID = "dataset-1"
	scope.EntityID = "entity-1"
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		TraceID:  "trace-async-write",
		Scope:    scope,
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage async error = %v", err)
	}
	if len(result.Jobs) != 1 || result.Jobs[0] == "" {
		t.Fatalf("AppendMessage Jobs = %+v, want one queued job", result.Jobs)
	}
	if len(jobStore.jobs) != 1 || jobStore.jobs[0].Scope != scope || jobStore.jobs[0].TraceID != "trace-async-write" {
		t.Fatalf("queued job = %+v, want full scope %+v and trace", jobStore.jobs, scope)
	}
	storedReport, ok, err := reportStore.GetLifecycleReport(ctx, "trace-async-write")
	if err != nil || !ok || storedReport.Status != memory.LifecycleStatusEnqueued || storedReport.JobID != result.Jobs[0] {
		t.Fatalf("async write enqueue report = %+v ok=%v err=%v, want enqueued trace report", storedReport, ok, err)
	}
	if len(result.Observations) != 0 || len(result.Facts) != 0 || result.FactGraph != nil {
		t.Fatalf("AppendMessage async sync outputs = %+v, want none before job store runs", result)
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
	storedReport, ok, err = reportStore.GetLifecycleReport(ctx, "trace-async-write")
	if err != nil || !ok || storedReport.Status != memory.LifecycleStatusCompleted || storedReport.TraceID != "trace-async-write" {
		t.Fatalf("async write completed report = %+v ok=%v err=%v, want completed trace report", storedReport, ok, err)
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

func TestMemoryFacadeAsyncWriteStagesRequireJobStore(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "async write stages require JobStore") {
		t.Fatalf("New async without job store err = %v, want job store validation error", err)
	}
}

func TestMemoryFacadeRunOnceDrainAndShutdownJobStore(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.JobStore = memory.NewMemoryJobStore()
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		WriteStages: []memory.StageSpec{{Name: "extract_observations", Async: true}},
	}, deps)
	if err != nil {
		t.Fatalf("New job store memory error = %v", err)
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

func TestMemoryFacadeCancelJobWritesReport(t *testing.T) {
	ctx := context.Background()
	jobStore := memory.NewMemoryJobStore()
	reportStore := memory.NewMemoryReportStore()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	deps.JobStore = jobStore
	deps.ReportStore = reportStore
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		WriteStages: []memory.StageSpec{{Name: "extract_observations", Async: true}},
	}, deps)
	if err != nil {
		t.Fatalf("New cancel job memory error = %v", err)
	}
	result, err := mem.AppendMessage(ctx, memory.AppendMessageRequest{
		TraceID:  "trace-cancel-append",
		Scope:    testScope("conv-cancel"),
		Messages: []sourcemessage.Message{messageWithText("Ada likes tea.")},
	})
	if err != nil {
		t.Fatalf("AppendMessage cancel setup error = %v", err)
	}
	if len(result.Jobs) != 1 {
		t.Fatalf("AppendMessage cancel jobs = %+v, want one job", result.Jobs)
	}
	if err := mem.CancelJob(ctx, result.Jobs[0], "operator cancelled"); err != nil {
		t.Fatalf("CancelJob error = %v", err)
	}
	stats, err := mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats after CancelJob error = %v", err)
	}
	if stats.Pending != 0 || stats.Cancelled != 1 || stats.CancelledByKind[memory.LifecycleJobKindWriteChain] != 1 {
		t.Fatalf("QueueStats after CancelJob = %+v, want cancelled write_chain job", stats)
	}
	reports, err := reportStore.ListLifecycleReports(ctx)
	if err != nil {
		t.Fatalf("ListLifecycleReports after CancelJob error = %v", err)
	}
	if len(reports) != 1 || reports[0].Status != memory.LifecycleStatusCancelled || reports[0].JobID != result.Jobs[0] || reports[0].TraceID != "trace-cancel-append" {
		t.Fatalf("CancelJob reports = %+v, want one cancelled report on append trace", reports)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, "trace-cancel-append")
	if err != nil || !ok || stored.Status != memory.LifecycleStatusCancelled || stored.JobID != result.Jobs[0] {
		t.Fatalf("CancelJob stored trace report = %+v ok=%v err=%v, want cancelled report", stored, ok, err)
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
	if len(pack.Items) != 1 || pack.Items[0].Kind != derive.ContextItemRecentMessage {
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

func TestMemoryFacadeDiagnosticsFreshnessReturnsStructuredChecks(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Diagnostics:  []memory.StageSpec{{Name: "freshness"}},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New diagnostics memory error = %v", err)
	}

	rawScope := memory.Scope{RuntimeID: " runtime-1 ", UserID: " user-1 ", ConversationID: " conv-1 "}
	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{Scope: rawScope})
	if err != nil {
		t.Fatalf("Diagnostics freshness error = %v", err)
	}
	wantScope := memory.Scope{RuntimeID: "runtime-1", UserID: "user-1", ConversationID: "conv-1"}
	if !report.Ready || !report.OK || report.Stage != "freshness" || report.Scope != wantScope {
		t.Fatalf("Diagnostics report = %+v, want ready/ok freshness with normalized scope %+v", report, wantScope)
	}
	if !reflect.DeepEqual(report.Capabilities, []memory.Capability{memory.CapabilityRecentWindow}) {
		t.Fatalf("Diagnostics capabilities = %+v, want recent_window default from assembly", report.Capabilities)
	}
	assertDiagnosticCheck(t, report, "system.configured", memory.DiagnosticStatusOK, true)
	assertDiagnosticCheck(t, report, "capability.recent_window.message_store", memory.DiagnosticStatusOK, true)
}

func TestMemoryFacadeDiagnosticsReportsMissingProjectionDependencies(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.Index = nil
	deps.ObservationStore = nil
	deps.ObservationExtractor = nil
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityObservationLedger},
		},
		Projections: []memory.ProjectionSpec{{
			Capability: memory.CapabilityObservationLedger,
			Namespace:  "observations",
		}},
		Diagnostics: []memory.StageSpec{{Name: "freshness"}},
	}, deps)
	if err != nil {
		t.Fatalf("New diagnostics missing deps memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        testScope("conv-1"),
		Capabilities: []memory.Capability{memory.CapabilityObservationLedger},
	})
	if err != nil {
		t.Fatalf("Diagnostics missing deps error = %v", err)
	}
	if report.Ready || report.OK {
		t.Fatalf("Diagnostics missing deps report = %+v, want not ready/ok", report)
	}
	assertDiagnosticCheck(t, report, "capability.observation_ledger.store", memory.DiagnosticStatusError, false)
	assertDiagnosticCheck(t, report, "capability.observation_ledger.service", memory.DiagnosticStatusError, false)
	assertDiagnosticCheck(t, report, "projection.observation_ledger.index", memory.DiagnosticStatusError, false)
	assertDiagnosticCheck(t, report, "projection.observation_ledger.scoped_namespace", memory.DiagnosticStatusOK, true)
}

func TestMemoryFacadeDiagnosticsUsesNormalizedHardPartitionScope(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityObservationLedger},
		},
		Projections: []memory.ProjectionSpec{{
			Capability: memory.CapabilityObservationLedger,
			Namespace:  "observations",
		}},
		Diagnostics: []memory.StageSpec{{Name: "freshness"}},
	}, deps)
	if err != nil {
		t.Fatalf("New diagnostics partition memory error = %v", err)
	}

	userReport, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        memory.Scope{RuntimeID: " runtime-1 ", UserID: " user-1 ", ConversationID: " conv-1 "},
		Capabilities: []memory.Capability{memory.CapabilityObservationLedger},
	})
	if err != nil {
		t.Fatalf("Diagnostics user scope error = %v", err)
	}
	globalReport, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        memory.Scope{RuntimeID: " runtime-1 ", ConversationID: " conv-1 "},
		Capabilities: []memory.Capability{memory.CapabilityObservationLedger},
	})
	if err != nil {
		t.Fatalf("Diagnostics global scope error = %v", err)
	}
	if userReport.Scope != testScope("conv-1") {
		t.Fatalf("Diagnostics user scope = %+v, want normalized %+v", userReport.Scope, testScope("conv-1"))
	}
	wantGlobalScope := testScope("conv-1")
	wantGlobalScope.UserID = ""
	if globalReport.Scope != wantGlobalScope {
		t.Fatalf("Diagnostics global scope = %+v, want normalized %+v", globalReport.Scope, wantGlobalScope)
	}
	userNamespace := diagnosticDetailString(t, assertDiagnosticCheck(t, userReport, "projection.observation_ledger.scoped_namespace", memory.DiagnosticStatusOK, true), "scoped_namespace")
	globalNamespace := diagnosticDetailString(t, assertDiagnosticCheck(t, globalReport, "projection.observation_ledger.scoped_namespace", memory.DiagnosticStatusOK, true), "scoped_namespace")
	if userNamespace == globalNamespace {
		t.Fatalf("scoped namespaces matched for user/global partitions: %q", userNamespace)
	}
}

func TestMemoryFacadeDiagnosticsRequiresDeclaredStage(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New diagnostics unavailable memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{Scope: testScope("conv-1")})
	if err == nil || !errdefs.IsNotAvailable(err) || report.OK || report.Ready {
		t.Fatalf("Diagnostics undeclared report=%+v err=%v, want undeclared NotAvailable", report, err)
	}
	assertDiagnosticCheck(t, report, "diagnostics.stage.freshness", memory.DiagnosticStatusError, false)
}

func TestMemoryFacadeDiagnosticsDeclaredStageWithoutProbeReturnsNotAvailable(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Diagnostics:  []memory.StageSpec{{Name: "queue_stats"}},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New diagnostics unsupported memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope: testScope("conv-1"),
		Stage: "queue_stats",
	})
	if err == nil || !errdefs.IsNotAvailable(err) || report.OK || report.Ready {
		t.Fatalf("Diagnostics unsupported report=%+v err=%v, want probe NotAvailable", report, err)
	}
	assertDiagnosticCheck(t, report, "diagnostics.stage.queue_stats", memory.DiagnosticStatusNotImplemented, false)
}

func TestMemoryFacadeDiagnosticsRegistryInvokesDeclaredProbe(t *testing.T) {
	ctx := context.Background()
	probe := &fakeDiagnosticProbe{}
	registry := memory.NewDiagnosticProbeRegistry()
	registry.Register("trace", "custom", probe)
	reportStore := memory.NewMemoryReportStore()
	deps := newDeps(t)
	deps.DiagnosticProbes = registry
	deps.ReportStore = reportStore
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Diagnostics:  []memory.StageSpec{{Name: "trace"}},
	}, deps)
	if err != nil {
		t.Fatalf("New diagnostics registry memory error = %v", err)
	}

	traceID := memory.TraceID("trace-diagnostic-custom")
	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		TraceID: traceID,
		Scope:   testScope("conv-1"),
		Stage:   "trace",
	})
	if err != nil {
		t.Fatalf("Diagnostics registry trace error = %v", err)
	}
	if report.TraceID != traceID || probe.calls != 1 || probe.stage != "trace" || probe.scope != testScope("conv-1") || probe.trace != traceID {
		t.Fatalf("custom probe report=%+v calls=%d stage=%q scope=%+v trace=%q, want one normalized trace call", report, probe.calls, probe.stage, probe.scope, probe.trace)
	}
	assertDiagnosticCheck(t, report, "diagnostics.probe.custom", memory.DiagnosticStatusOK, true)
	stored, ok, err := reportStore.GetDiagnosticReport(ctx, traceID)
	if err != nil || !ok || stored.TraceID != traceID || stored.Stage != "trace" {
		t.Fatalf("stored diagnostic report = %+v ok=%v err=%v, want trace report", stored, ok, err)
	}
}

func TestMemoryFacadeTraceDiagnosticsReturnsStructuredDetails(t *testing.T) {
	ctx := context.Background()
	deps := newDeps(t)
	deps.ObservationExtractor = &fakeObservationExtractor{}
	mem, err := memory.New(memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityObservationLedger, Required: true},
		},
		Projections: []memory.ProjectionSpec{{
			Capability: memory.CapabilityObservationLedger,
			Namespace:  "observations",
		}},
		Diagnostics: []memory.StageSpec{{Name: "trace"}},
	}, deps)
	if err != nil {
		t.Fatalf("New trace diagnostics memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        testScope("conv-1"),
		Stage:        "trace",
		Capabilities: []memory.Capability{memory.CapabilityObservationLedger},
	})
	if err != nil {
		t.Fatalf("Diagnostics trace error = %v", err)
	}
	if !report.Ready || !report.OK || report.Stage != "trace" || report.TraceID == "" {
		t.Fatalf("trace report = %+v, want ready/ok trace", report)
	}
	assertDiagnosticCheck(t, report, "diagnostics.stage.trace", memory.DiagnosticStatusOK, true)
	planCheck := assertDiagnosticCheck(t, report, "trace.plan", memory.DiagnosticStatusOK, true)
	if got := planCheck.Details["diagnostic_stages"]; got == nil {
		t.Fatalf("trace.plan diagnostic_stages missing: %+v", planCheck)
	}
	projectionCheck := assertDiagnosticCheck(t, report, "trace.projections", memory.DiagnosticStatusOK, true)
	projections, ok := projectionCheck.Details[string(memory.CapabilityObservationLedger)].(map[string]any)
	if !ok || projections["scoped_namespace"] == "" || projections["filter"] == nil {
		t.Fatalf("trace.projections details = %+v, want scoped namespace and filter for observation ledger", projectionCheck.Details)
	}
}

func TestMemoryFacadeConsistencyDiagnosticsRequiresDeclaredStage(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	mem, err := memory.New(documentRetrievalSpec(), deps)
	if err != nil {
		t.Fatalf("New consistency undeclared memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        documentScope("dataset-1"),
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
	})
	if err == nil || !errdefs.IsNotAvailable(err) || report.OK || report.Ready {
		t.Fatalf("Consistency diagnostics undeclared report=%+v err=%v, want NotAvailable", report, err)
	}
	assertDiagnosticCheck(t, report, "diagnostics.stage.consistency", memory.DiagnosticStatusError, false)
}

func TestMemoryFacadeConsistencyDiagnosticsRequiresExplicitCapabilities(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency empty capability memory error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope: documentScope("dataset-1"),
		Stage: "consistency",
	})
	if err != nil {
		t.Fatalf("Consistency empty capability diagnostics error = %v", err)
	}
	check := assertDiagnosticCheck(t, report, "consistency.capabilities", memory.DiagnosticStatusError, false)
	if check.RepairHint == "" {
		t.Fatalf("consistency.capabilities RepairHint empty: %+v", check)
	}
}

func TestMemoryFacadeConsistencyProjectionHydratesDocumentChunks(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency projection memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-ok", Content: "consistent document chunk projection"},
	}); err != nil {
		t.Fatalf("ImportDocument consistency ok error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("Consistency projection diagnostics error = %v", err)
	}
	if !report.OK || !report.Ready || report.NextPageToken != "" {
		t.Fatalf("Consistency projection report = %+v, want ok without next page", report)
	}
	check := assertDiagnosticCheck(t, report, "consistency.projection.document_chunks.record", memory.DiagnosticStatusOK, true)
	if check.RepairHint != "" || check.Details["hydrate_state"] != "found" {
		t.Fatalf("projection consistency check = %+v, want hydrated without repair hint", check)
	}
}

func TestMemoryFacadeConsistencyProjectionReportsMissingSemanticRecord(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency missing semantic memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-missing-semantic", Content: "missing semantic document chunk"},
	}); err != nil {
		t.Fatalf("ImportDocument missing semantic error = %v", err)
	}
	if err := deps.ChunkStore.DeleteDocument(ctx, scope, "doc-missing-semantic"); err != nil {
		t.Fatalf("DeleteDocument missing semantic error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
	})
	if err != nil {
		t.Fatalf("Consistency missing semantic diagnostics error = %v", err)
	}
	check := assertDiagnosticCheck(t, report, "consistency.projection.document_chunks.record", memory.DiagnosticStatusMissing, false)
	if !strings.Contains(check.RepairHint, "reload document_chunks target=dataset-1/doc-missing-semantic") {
		t.Fatalf("missing semantic RepairHint = %q, want reload target hint", check.RepairHint)
	}
}

func TestMemoryFacadeConsistencyProjectionReportsMismatchedScopeMetadata(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency stale scope memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-stale-scope", Content: "stale projection scope metadata"},
	}); err != nil {
		t.Fatalf("ImportDocument stale scope error = %v", err)
	}
	namespace := scopedProjectionNamespace(t, mem, memory.CapabilityDocumentChunks, scope)
	resp, err := deps.Index.List(ctx, namespace, retrieval.ListRequest{PageSize: 10, OrderBy: retrieval.OrderByIDAsc})
	if err != nil || len(resp.Items) != 1 {
		t.Fatalf("List projection docs len=%d err=%v, want one", len(resp.Items), err)
	}
	stale := resp.Items[0]
	stale.Metadata[projectors.MetadataUserIDKey] = "other-user"
	if err := deps.Index.Upsert(ctx, namespace, []retrieval.Doc{stale}); err != nil {
		t.Fatalf("Upsert stale projection metadata error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
	})
	if err != nil {
		t.Fatalf("Consistency stale scope diagnostics error = %v", err)
	}
	check := assertDiagnosticCheck(t, report, "consistency.projection.document_chunks.record", memory.DiagnosticStatusStale, false)
	if check.RepairHint == "" || check.Details["scope_mismatches"] == nil {
		t.Fatalf("stale scope check = %+v, want mismatch details and repair hint", check)
	}
}

func TestMemoryFacadeConsistencySourceViewReportsStaleDocumentChunk(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency stale source view memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-stale-source", Content: "fresh source view content"},
	}); err != nil {
		t.Fatalf("ImportDocument stale source error = %v", err)
	}
	if _, err := deps.DocumentStore.Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-stale-source",
		Content:   "updated source view content",
	}}); err != nil {
		t.Fatalf("Put updated canonical document error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckSourceView},
	})
	if err != nil {
		t.Fatalf("Consistency stale source view diagnostics error = %v", err)
	}
	check := assertDiagnosticCheck(t, report, "consistency.source_view.document_chunks.record", memory.DiagnosticStatusStale, false)
	if !strings.Contains(check.RepairHint, "reload document_chunks target=dataset-1/doc-stale-source") {
		t.Fatalf("stale source RepairHint = %q, want reload target hint", check.RepairHint)
	}
}

func TestMemoryFacadeConsistencySourceViewReportsMissingDocument(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency missing source document memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope:    scope,
		Document: sourcedocument.Document{ID: "doc-missing-source", Content: "missing canonical source view"},
	}); err != nil {
		t.Fatalf("ImportDocument missing source error = %v", err)
	}
	documentStore, ok := deps.DocumentStore.(*sourcedocument.WorkspaceStore)
	if !ok {
		t.Fatalf("DocumentStore type = %T, want *WorkspaceStore", deps.DocumentStore)
	}
	if err := documentStore.Delete(ctx, "dataset-1", "doc-missing-source"); err != nil {
		t.Fatalf("Delete canonical document error = %v", err)
	}

	report, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckSourceView},
	})
	if err != nil {
		t.Fatalf("Consistency missing source diagnostics error = %v", err)
	}
	check := assertDiagnosticCheck(t, report, "consistency.source_view.document_chunks.record", memory.DiagnosticStatusMissing, false)
	if check.RepairHint == "" || check.Details["state"] != "missing_document" {
		t.Fatalf("missing document check = %+v, want state and repair hint", check)
	}
}

func TestMemoryFacadeConsistencyPaginationReturnsNextPageToken(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Diagnostics = []memory.StageSpec{{Name: "consistency"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New consistency pagination memory error = %v", err)
	}
	scope := documentScope("dataset-1")
	for _, id := range []string{"doc-page-1", "doc-page-2"} {
		if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
			Scope:    scope,
			Document: sourcedocument.Document{ID: id, Content: "paginated consistency " + id},
		}); err != nil {
			t.Fatalf("ImportDocument pagination %s error = %v", id, err)
		}
	}

	first, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
		PageSize:     1,
	})
	if err != nil {
		t.Fatalf("Consistency first page diagnostics error = %v", err)
	}
	if first.NextPageToken == "" {
		t.Fatalf("first consistency page NextPageToken empty; report=%+v", first)
	}
	assertDiagnosticCheck(t, first, "consistency.projection.document_chunks.record", memory.DiagnosticStatusOK, true)

	second, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Stage:        "consistency",
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Consistency:  []memory.ConsistencyCheckKind{memory.ConsistencyCheckProjection},
		PageSize:     1,
		PageToken:    first.NextPageToken,
	})
	if err != nil {
		t.Fatalf("Consistency second page diagnostics error = %v", err)
	}
	if second.NextPageToken != "" {
		t.Fatalf("second consistency page NextPageToken = %q, want empty", second.NextPageToken)
	}
	assertDiagnosticCheck(t, second, "consistency.projection.document_chunks.record", memory.DiagnosticStatusOK, true)
}

func TestMemoryFacadeFreshnessDryRunIncludesDiagnosticChecks(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Lifecycle: []memory.StageSpec{
			{Name: "freshness_check"},
		},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New freshness diagnostics memory error = %v", err)
	}

	result, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope:  memory.Scope{RuntimeID: " runtime-1 ", UserID: " user-1 ", ConversationID: " conv-1 "},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("Freshness dry-run error = %v", err)
	}
	if result.Operation.ID == "" || !result.Supported || !result.Accepted || result.Status != memory.LifecycleStatusPlanned || !result.Operation.DryRun || result.Operation.Action != memory.LifecycleActionFreshnessCheck || !result.Ready || !result.OK {
		t.Fatalf("Freshness dry-run result = %+v, want lifecycle plus ready diagnostics", result)
	}
	if result.Operation.Scope != testScope("conv-1") || result.Diagnostics.Scope != testScope("conv-1") {
		t.Fatalf("Freshness scopes = lifecycle %+v diagnostics %+v, want normalized", result.Operation.Scope, result.Diagnostics.Scope)
	}
	assertDiagnosticCheck(t, result.Diagnostics, "diagnostics.stage.freshness", memory.DiagnosticStatusOK, true)
	assertFreshnessCheck(t, result, "capability.recent_window.message_store", memory.DiagnosticStatusOK, true)
}

func TestMemoryFacadeDocumentTargetFreshnessReportsFreshStaleAndMissing(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Lifecycle = []memory.StageSpec{{Name: "freshness_check"}}
	spec.Diagnostics = []memory.StageSpec{{Name: "freshness"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New document freshness memory error = %v", err)
	}
	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			ID:      "doc-fresh",
			Content: "fresh document target content",
		},
	}); err != nil {
		t.Fatalf("ImportDocument fresh error = %v", err)
	}

	fresh, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-fresh"}},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Freshness fresh target error = %v", err)
	}
	freshCheck := assertFreshnessCheck(t, fresh, "freshness.document_chunks.target", memory.DiagnosticStatusOK, true)
	if state := freshCheck.Details["state"]; state != "fresh" {
		t.Fatalf("fresh state = %#v, want fresh; check=%+v", state, freshCheck)
	}
	if got := freshCheck.Details["chunk_count"]; got != 1 {
		t.Fatalf("fresh chunk_count = %#v, want 1", got)
	}
	diagnostics, err := mem.Diagnostics(ctx, memory.DiagnosticRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-fresh"}},
	})
	if err != nil {
		t.Fatalf("Diagnostics targeted freshness error = %v", err)
	}
	assertDiagnosticCheck(t, diagnostics, "freshness.document_chunks.target", memory.DiagnosticStatusOK, true)

	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-fresh",
		Content:   "updated canonical document target content",
	}}); err != nil {
		t.Fatalf("Put stale canonical document error = %v", err)
	}
	stale, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-fresh"}},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Freshness stale target error = %v", err)
	}
	if stale.OK || stale.Ready {
		t.Fatalf("stale freshness = %+v, want not ok/ready", stale)
	}
	assertFreshnessCheck(t, stale, "freshness.document_chunks.target", memory.DiagnosticStatusStale, false)

	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-no-chunks",
		Content:   "canonical document without chunks",
	}}); err != nil {
		t.Fatalf("Put missing chunks document error = %v", err)
	}
	missingChunks, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-no-chunks"}},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Freshness missing chunks error = %v", err)
	}
	missingChunksCheck := assertFreshnessCheck(t, missingChunks, "freshness.document_chunks.target", memory.DiagnosticStatusMissing, false)
	if state := missingChunksCheck.Details["state"]; state != "missing_chunks" {
		t.Fatalf("missing chunks state = %#v, want missing_chunks", state)
	}

	missingDoc, err := mem.Freshness(ctx, memory.FreshnessRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-missing"}},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Freshness missing document error = %v", err)
	}
	missingDocCheck := assertFreshnessCheck(t, missingDoc, "freshness.document_chunks.target", memory.DiagnosticStatusMissing, false)
	if state := missingDocCheck.Details["state"]; state != "missing_document" {
		t.Fatalf("missing document state = %#v, want missing_document", state)
	}
}

func TestMemoryFacadeDocumentTargetReloadAndRebuildDryRunPlansTargets(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	spec := documentRetrievalSpec()
	spec.Lifecycle = []memory.StageSpec{{Name: "reload"}, {Name: "rebuild"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New document lifecycle memory error = %v", err)
	}
	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"
	targets := []memory.DocumentTarget{{DocumentID: "doc-1"}}

	reload, err := mem.Reload(ctx, memory.ReloadRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    targets,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Reload dry-run target error = %v", err)
	}
	if reload.Operation.ID == "" || reload.Operation.Action != memory.LifecycleActionReload || !reload.Operation.DryRun || reload.Status != memory.LifecycleStatusPlanned || !reload.Accepted || !reload.Supported || len(reload.Steps) != 1 || reload.Steps[0].Completed {
		t.Fatalf("Reload dry-run = %+v, want one planned target step", reload)
	}
	if reload.Operation.Documents[0] != (memory.DocumentTarget{DatasetID: "dataset-1", DocumentID: "doc-1"}) {
		t.Fatalf("Reload documents = %+v, want normalized dataset target", reload.Operation.Documents)
	}
	if len(reload.Operation.Targets) != 1 || reload.Operation.Targets[0].DocumentID != "doc-1" || reload.Operation.Targets[0].DatasetID != "dataset-1" {
		t.Fatalf("Reload targets = %+v, want normalized document target", reload.Operation.Targets)
	}
	if got := reload.Steps[0].Details["chunk_count"]; got != 0 {
		t.Fatalf("Reload dry-run chunk_count = %#v, want 0", got)
	}

	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    targets,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Rebuild dry-run target error = %v", err)
	}
	if rebuild.Operation.ID == "" || rebuild.Operation.Action != memory.LifecycleActionRebuild || !rebuild.Operation.DryRun || rebuild.Status != memory.LifecycleStatusPlanned || !rebuild.Accepted || !rebuild.Supported || len(rebuild.Steps) != 1 || rebuild.Steps[0].Completed {
		t.Fatalf("Rebuild dry-run = %+v, want one planned target step", rebuild)
	}

	noTargets, err := mem.Reload(ctx, memory.ReloadRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Reload dry-run no targets error = %v", err)
	}
	if !noTargets.Accepted || noTargets.Status != memory.LifecycleStatusSkipped || len(noTargets.Steps) != 1 || !strings.Contains(noTargets.Message, "no full scan") {
		t.Fatalf("Reload dry-run no targets = %+v, want explicit no-full-scan plan", noTargets)
	}
}

func TestMemoryFacadeDocumentTargetReloadAndRebuildDrainReindexesUpdatedText(t *testing.T) {
	ctx := context.Background()
	chunker := &fakeDocumentChunker{}
	deps := newDocumentDeps(t)
	deps.DocumentChunker = chunker
	deps.JobStore = memory.NewMemoryJobStore()
	reportStore := memory.NewMemoryReportStore()
	deps.ReportStore = reportStore
	spec := documentRetrievalSpec()
	spec.Lifecycle = []memory.StageSpec{{Name: "reload"}, {Name: "rebuild"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New document reload memory error = %v", err)
	}
	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"
	if _, err := mem.ImportDocument(ctx, memory.ImportDocumentRequest{
		Scope: scope,
		Document: sourcedocument.Document{
			ID:      "doc-1",
			Content: "old reload target text",
		},
	}); err != nil {
		t.Fatalf("ImportDocument initial error = %v", err)
	}
	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-1",
		Content:   "updated reload target needle text",
	}}); err != nil {
		t.Fatalf("Put updated canonical document error = %v", err)
	}

	traceID := memory.TraceID("trace-reload-doc-1")
	reload, err := mem.Reload(ctx, memory.ReloadRequest{
		TraceID:      traceID,
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-1"}},
	})
	if err != nil {
		t.Fatalf("Reload target error = %v", err)
	}
	if !reload.Accepted || reload.Status != memory.LifecycleStatusEnqueued || reload.JobID == "" || len(reload.Steps) != 1 || reload.Steps[0].Completed {
		t.Fatalf("Reload target report = %+v, want enqueued target job", reload)
	}
	if reload.TraceID != traceID || reload.Operation.TraceID != traceID {
		t.Fatalf("Reload trace = report %q operation %q, want %q", reload.TraceID, reload.Operation.TraceID, traceID)
	}
	stored, ok, err := reportStore.GetLifecycleReport(ctx, traceID)
	if err != nil || !ok || stored.Status != memory.LifecycleStatusEnqueued || stored.JobID != reload.JobID {
		t.Fatalf("stored reload dispatch report = %+v ok=%v err=%v, want enqueued report", stored, ok, err)
	}
	if chunker.calls != 1 {
		t.Fatalf("chunker calls before reload drain = %d, want import only", chunker.calls)
	}
	run, err := mem.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce reload target error = %v", err)
	}
	if !run.Completed || run.JobID != reload.JobID || run.Kind != memory.LifecycleJobKindReload {
		t.Fatalf("RunOnce reload result = %+v, want completed reload job", run)
	}
	stored, ok, err = reportStore.GetLifecycleReport(ctx, traceID)
	if err != nil || !ok || stored.Status != memory.LifecycleStatusCompleted || stored.JobID != reload.JobID || stored.TraceID != traceID {
		t.Fatalf("stored reload run report = %+v ok=%v err=%v, want completed trace report", stored, ok, err)
	}
	if chunker.calls != 2 {
		t.Fatalf("chunker calls after reload drain = %d, want import plus reload", chunker.calls)
	}
	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: "updated reload needle",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("PackContext after reload error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || !strings.Contains(pack.DocumentHits[0].Chunk.Text, "updated reload target needle text") {
		t.Fatalf("DocumentHits after reload = %+v, want updated chunk text", pack.DocumentHits)
	}

	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-1",
		Content:   "updated rebuild target needle text",
	}}); err != nil {
		t.Fatalf("Put rebuild canonical document error = %v", err)
	}
	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-1"}},
	})
	if err != nil {
		t.Fatalf("Rebuild target error = %v", err)
	}
	if !rebuild.Accepted || rebuild.Status != memory.LifecycleStatusEnqueued || rebuild.JobID == "" {
		t.Fatalf("Rebuild target report = %+v, want enqueued target job", rebuild)
	}
	if err := mem.Drain(ctx); err != nil {
		t.Fatalf("Drain rebuild target error = %v", err)
	}
	if chunker.calls != 3 {
		t.Fatalf("chunker calls after rebuild drain = %d, want import plus reload plus rebuild", chunker.calls)
	}
	pack, err = mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: "updated rebuild needle",
		TopK:  5,
	})
	if err != nil {
		t.Fatalf("PackContext after rebuild error = %v", err)
	}
	if len(pack.DocumentHits) != 1 || !strings.Contains(pack.DocumentHits[0].Chunk.Text, "updated rebuild target needle text") {
		t.Fatalf("DocumentHits after rebuild = %+v, want updated chunk text", pack.DocumentHits)
	}
}

func TestMemoryFacadeDocumentTargetRunnerErrorMarksJobFailed(t *testing.T) {
	ctx := context.Background()
	jobStore := memory.NewMemoryJobStore()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	deps.JobStore = jobStore
	spec := documentRetrievalSpec()
	spec.Lifecycle = []memory.StageSpec{{Name: "reload"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New document reload failure memory error = %v", err)
	}
	scope := testScope("conv-doc")
	scope.DatasetID = "dataset-1"

	reload, err := mem.Reload(ctx, memory.ReloadRequest{
		Scope:        scope,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-missing"}},
	})
	if err != nil {
		t.Fatalf("Reload missing target enqueue error = %v", err)
	}
	if reload.Status != memory.LifecycleStatusEnqueued || reload.JobID == "" {
		t.Fatalf("Reload missing target report = %+v, want enqueued job", reload)
	}
	result, err := mem.RunOnce(ctx)
	if err == nil {
		t.Fatalf("RunOnce missing target err = nil, want runner error; result=%+v", result)
	}
	if result.Completed || result.JobID != reload.JobID || result.Error == "" {
		t.Fatalf("RunOnce missing target result = %+v, want failed job result", result)
	}
	stats, err := mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats after failed reload error = %v", err)
	}
	if stats.Pending != 0 || stats.Completed != 0 || stats.Failed != 1 {
		t.Fatalf("QueueStats after failed reload = %+v, want one failed job", stats)
	}
}

func TestMemoryFacadeDocumentTargetReloadKeepsRuntimeUserHardPartitions(t *testing.T) {
	ctx := context.Background()
	deps := newDocumentDeps(t)
	deps.DocumentChunker = &fakeDocumentChunker{}
	deps.JobStore = memory.NewMemoryJobStore()
	spec := documentRetrievalSpec()
	spec.Lifecycle = []memory.StageSpec{{Name: "reload"}}
	mem, err := memory.New(spec, deps)
	if err != nil {
		t.Fatalf("New partitioned reload memory error = %v", err)
	}
	userOne := testScope("conv-doc")
	userOne.DatasetID = "dataset-1"
	userTwo := userOne
	userTwo.UserID = "user-2"

	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-1",
		Content:   "user one partition text",
	}}); err != nil {
		t.Fatalf("Put user one canonical document error = %v", err)
	}
	if _, err := mem.Reload(ctx, memory.ReloadRequest{
		Scope:        userOne,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-1"}},
	}); err != nil {
		t.Fatalf("Reload user one target error = %v", err)
	}
	if err := mem.Drain(ctx); err != nil {
		t.Fatalf("Drain user one reload error = %v", err)
	}
	if _, err := mem.DocumentStore().Put(ctx, sourcedocument.PutRequest{Document: sourcedocument.Document{
		DatasetID: "dataset-1",
		ID:        "doc-1",
		Content:   "user two partition text",
	}}); err != nil {
		t.Fatalf("Put user two canonical document error = %v", err)
	}
	if _, err := mem.Reload(ctx, memory.ReloadRequest{
		Scope:        userTwo,
		Capabilities: []memory.Capability{memory.CapabilityDocumentChunks},
		Documents:    []memory.DocumentTarget{{DocumentID: "doc-1"}},
	}); err != nil {
		t.Fatalf("Reload user two target error = %v", err)
	}
	if err := mem.Drain(ctx); err != nil {
		t.Fatalf("Drain user two reload error = %v", err)
	}

	userOnePack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: userOne, Query: "user one partition", TopK: 5})
	if err != nil {
		t.Fatalf("PackContext user one error = %v", err)
	}
	if len(userOnePack.DocumentHits) != 1 || !strings.Contains(userOnePack.DocumentHits[0].Chunk.Text, "user one partition text") {
		t.Fatalf("user one DocumentHits = %+v, want user one partition chunk", userOnePack.DocumentHits)
	}
	userTwoPack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: userTwo, Query: "user two partition", TopK: 5})
	if err != nil {
		t.Fatalf("PackContext user two error = %v", err)
	}
	if len(userTwoPack.DocumentHits) != 1 || !strings.Contains(userTwoPack.DocumentHits[0].Chunk.Text, "user two partition text") {
		t.Fatalf("user two DocumentHits = %+v, want user two partition chunk", userTwoPack.DocumentHits)
	}
}

func TestMemoryFacadeRebuildAndReconcileDryRunReturnPlannedReports(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Lifecycle: []memory.StageSpec{
			{Name: "readiness"},
			{Name: "rebuild"},
			{Name: "reconcile"},
		},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New dry-run lifecycle memory error = %v", err)
	}

	rawScope := memory.Scope{
		RuntimeID:      " runtime-1 ",
		UserID:         " user-1 ",
		AgentID:        " agent-1 ",
		ConversationID: " conv-1 ",
		DatasetID:      " dataset-1 ",
		EntityID:       " entity-1 ",
	}
	wantScope := memory.Scope{
		RuntimeID:      "runtime-1",
		UserID:         "user-1",
		AgentID:        "agent-1",
		ConversationID: "conv-1",
		DatasetID:      "dataset-1",
		EntityID:       "entity-1",
	}
	capabilities := []memory.Capability{memory.CapabilityObservationLedger, memory.CapabilityFactGraph}
	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        rawScope,
		Capabilities: capabilities,
		DryRun:       true,
		Reason:       " operator requested ",
	})
	if err != nil {
		t.Fatalf("Rebuild dry-run error = %v", err)
	}
	if rebuild.Operation.ID == "" || rebuild.Operation.PlanDigest == "" || rebuild.Operation.IdempotencyKey == "" || !rebuild.Supported || !rebuild.Accepted || rebuild.Status != memory.LifecycleStatusPlanned || !rebuild.Operation.DryRun || rebuild.Operation.Action != memory.LifecycleActionRebuild {
		t.Fatalf("Rebuild dry-run report = %+v, want supported accepted dry-run rebuild", rebuild)
	}
	if rebuild.Operation.Scope != wantScope || !reflect.DeepEqual(rebuild.Operation.Capabilities, capabilities) {
		t.Fatalf("Rebuild dry-run scope/capabilities = %+v/%+v, want %+v/%+v", rebuild.Operation.Scope, rebuild.Operation.Capabilities, wantScope, capabilities)
	}
	if rebuild.JobID != "" || rebuild.Operation.Reason != "operator requested" || !strings.Contains(rebuild.Message, "planned") {
		t.Fatalf("Rebuild dry-run job/reason/message = %+v/%q/%q, want no job, trimmed reason, planned message", rebuild.JobID, rebuild.Operation.Reason, rebuild.Message)
	}
	if len(rebuild.Steps) != 1 || !rebuild.Steps[0].Planned || rebuild.Steps[0].Completed {
		t.Fatalf("Rebuild dry-run steps = %+v, want one planned substrate step", rebuild.Steps)
	}

	reconcile, err := mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        rawScope,
		Capabilities: []memory.Capability{memory.CapabilityFactLedger},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Reconcile dry-run error = %v", err)
	}
	if reconcile.Operation.ID == "" || !reconcile.Supported || !reconcile.Accepted || reconcile.Status != memory.LifecycleStatusPlanned || !reconcile.Operation.DryRun || reconcile.Operation.Action != memory.LifecycleActionReconcile {
		t.Fatalf("Reconcile dry-run report = %+v, want supported accepted dry-run reconcile", reconcile)
	}
	if reconcile.Operation.Scope != wantScope || !reflect.DeepEqual(reconcile.Operation.Capabilities, []memory.Capability{memory.CapabilityFactLedger}) {
		t.Fatalf("Reconcile dry-run scope/capabilities = %+v/%+v, want %+v/fact_ledger", reconcile.Operation.Scope, reconcile.Operation.Capabilities, wantScope)
	}
}

func TestMemoryFacadeNoRunnerLifecycleActionDoesNotEnqueue(t *testing.T) {
	ctx := context.Background()
	jobStore := newRecordingJobStore()
	deps := newDeps(t)
	deps.JobStore = jobStore
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
		Lifecycle: []memory.StageSpec{
			{Name: "rebuild"},
			{Name: "reconcile"},
		},
	}, deps)
	if err != nil {
		t.Fatalf("New lifecycle job store memory error = %v", err)
	}
	rawScope := memory.Scope{
		RuntimeID:      " runtime-1 ",
		UserID:         " user-1 ",
		AgentID:        " agent-1 ",
		ConversationID: " conv-1 ",
		DatasetID:      " dataset-1 ",
		EntityID:       " entity-1 ",
	}
	wantScope := memory.Scope{
		RuntimeID:      "runtime-1",
		UserID:         "user-1",
		AgentID:        "agent-1",
		ConversationID: "conv-1",
		DatasetID:      "dataset-1",
		EntityID:       "entity-1",
	}
	rebuildCapabilities := []memory.Capability{memory.CapabilityObservationLedger, memory.CapabilityFactGraph}
	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{
		Scope:        rawScope,
		Capabilities: rebuildCapabilities,
		Reason:       "rebuild substrate",
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Rebuild no-runner err = %v, want NotAvailable", err)
	}
	if !rebuild.Supported || rebuild.Accepted || rebuild.Status != memory.LifecycleStatusUnsupported || rebuild.Operation.DryRun || rebuild.JobID != "" {
		t.Fatalf("Rebuild no-runner report = %+v, want unsupported without job", rebuild)
	}
	reconcile, err := mem.Reconcile(ctx, memory.ReconcileRequest{
		Scope:        rawScope,
		Capabilities: []memory.Capability{memory.CapabilityFactLedger},
		Reason:       "reconcile substrate",
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("Reconcile no-runner err = %v, want NotAvailable", err)
	}
	if !reconcile.Supported || reconcile.Accepted || reconcile.Status != memory.LifecycleStatusUnsupported || reconcile.Operation.DryRun || reconcile.JobID != "" {
		t.Fatalf("Reconcile no-runner report = %+v, want unsupported without job", reconcile)
	}
	if rebuild.Operation.Scope != wantScope || !reflect.DeepEqual(rebuild.Operation.Capabilities, rebuildCapabilities) {
		t.Fatalf("rebuild normalized scope/capabilities = %+v/%+v, want %+v/%+v", rebuild.Operation.Scope, rebuild.Operation.Capabilities, wantScope, rebuildCapabilities)
	}
	stats, err := mem.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats after no-runner lifecycle error = %v", err)
	}
	if len(jobStore.jobs) != 0 || stats.Pending != 0 || stats.Completed != 0 || stats.Failed != 0 {
		t.Fatalf("no-runner jobs=%d stats=%+v, want nothing enqueued", len(jobStore.jobs), stats)
	}
}

func TestMemoryFacadeRebuildAndReconcileRequireDeclaredLifecycleStages(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.New(memory.Spec{
		Sources:      []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{{Capability: memory.CapabilityRecentWindow, Required: true}},
	}, newDeps(t))
	if err != nil {
		t.Fatalf("New lifecycle unavailable memory error = %v", err)
	}
	rebuild, err := mem.Rebuild(ctx, memory.RebuildRequest{Scope: testScope("conv-1")})
	if err == nil || !errdefs.IsNotAvailable(err) || rebuild.Supported || rebuild.Accepted || rebuild.Status != memory.LifecycleStatusUnsupported || rebuild.Operation.Action != memory.LifecycleActionRebuild {
		t.Fatalf("Rebuild result=%+v err=%v, want undeclared NotAvailable", rebuild, err)
	}
	reconcile, err := mem.Reconcile(ctx, memory.ReconcileRequest{Scope: testScope("conv-1")})
	if err == nil || !errdefs.IsNotAvailable(err) || reconcile.Supported || reconcile.Accepted || reconcile.Status != memory.LifecycleStatusUnsupported || reconcile.Operation.Action != memory.LifecycleActionReconcile {
		t.Fatalf("Reconcile result=%+v err=%v, want undeclared NotAvailable", reconcile, err)
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

func TestPublicRootDoesNotReExportInternalExecutorOrDeriveDTOs(t *testing.T) {
	const executorImportPath = "github.com/GizClaw/flowcraft/memory/internal/executor"

	forbiddenExecutorAliases := map[string]bool{
		"DocumentChunkSearchResponse":  true,
		"SummaryNodeSearchResponse":    true,
		"ObservationSearchResponse":    true,
		"FactSearchResponse":           true,
		"FactGraphBuildResult":         true,
		"FactGraphSearchResponse":      true,
		"EntityProfileSearchResponse":  true,
		"EntityTimelineSearchResponse": true,
		"ContextPack":                  true,
	}
	forbiddenRootDeriveTypes := map[string]bool{
		"DocumentChunker":         true,
		"DocumentChunkInput":      true,
		"Summarizer":              true,
		"SummaryInput":            true,
		"ObservationExtractor":    true,
		"ObservationInput":        true,
		"FactReconciler":          true,
		"FactReconcileInput":      true,
		"FactGraphBuilder":        true,
		"FactGraphInput":          true,
		"FactGraphOutput":         true,
		"EntityProfileBuilder":    true,
		"EntityProfileInput":      true,
		"EntityTimelineBuilder":   true,
		"EntityTimelineInput":     true,
		"ContextPacker":           true,
		"DocumentChunkSearchHit":  true,
		"SummaryNodeSearchHit":    true,
		"ObservationSearchHit":    true,
		"FactSearchHit":           true,
		"FactGraphSearchHit":      true,
		"EntityProfileSearchHit":  true,
		"EntityTimelineSearchHit": true,
		"ContextPackInput":        true,
		"ContextPackOutput":       true,
		"ContextItemKind":         true,
		"ContextItem":             true,
	}
	forbiddenRootDeriveConstants := map[string]bool{
		"ContextItemRecentMessage":  true,
		"ContextItemSummaryNode":    true,
		"ContextItemDocumentChunk":  true,
		"ContextItemObservation":    true,
		"ContextItemFact":           true,
		"ContextItemFactGraphNode":  true,
		"ContextItemFactGraphEdge":  true,
		"ContextItemEntityProfile":  true,
		"ContextItemEntityTimeline": true,
	}

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
			name := filepath.Base(path)
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
					if forbiddenRootDeriveTypes[name] {
						t.Fatalf("public derive type alias %s must not be re-exported from root memory package; import memory/derive directly (found in %s)", name, filename)
					}
					if forbiddenExecutorAliases[name] {
						selector, ok := typeSpec.Type.(*ast.SelectorExpr)
						if !ok || !isImportedSelector(selector, executorImports) {
							continue
						}
						t.Fatalf("public type alias %s must not re-export internal executor DTO %s.%s (found in %s)", name, selector.X.(*ast.Ident).Name, selector.Sel.Name, filename)
					}
				}
			case token.CONST:
				for _, spec := range gen.Specs {
					valueSpec := spec.(*ast.ValueSpec)
					for _, nameIdent := range valueSpec.Names {
						if !nameIdent.IsExported() {
							continue
						}
						if forbiddenRootDeriveConstants[nameIdent.Name] {
							t.Fatalf("public derive constant alias %s must not be re-exported from root memory package; import memory/derive directly (found in %s)", nameIdent.Name, filename)
						}
					}
				}
			}
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
				if !typeSpec.Name.IsExported() {
					continue
				}
				guardNamespaceFields := strings.HasSuffix(name, "Request") || name == "ContextPackInput" || name == "ContextPackOutput"
				if !guardNamespaceFields {
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

func isImportedSelector(selector *ast.SelectorExpr, imports map[string]bool) bool {
	ident, ok := selector.X.(*ast.Ident)
	return ok && imports[ident.Name]
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

type recordingJobStore struct {
	*memory.MemoryJobStore
	jobs []memory.LifecycleJob
}

func newRecordingJobStore() *recordingJobStore {
	return &recordingJobStore{MemoryJobStore: memory.NewMemoryJobStore()}
}

func (s *recordingJobStore) Enqueue(ctx context.Context, job memory.LifecycleJob) (memory.LifecycleJobID, error) {
	s.jobs = append(s.jobs, job)
	return s.MemoryJobStore.Enqueue(ctx, job)
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
		MessageStore:        sourcemessage.NewWorkspaceStore(sdkworkspace.Sub(ws, "sources/message")),
		ObservationStore:    viewobservation.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/observation_ledger")),
		FactStore:           fact.NewLedgerWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_ledger")),
		FactGraphStore:      fact.NewGraphWorkspaceStore(sdkworkspace.Sub(ws, "views/fact_graph")),
		EntityProfileStore:  viewentity.NewProfileWorkspaceStore(sdkworkspace.Sub(ws, "views/entity_profile")),
		EntityTimelineStore: viewentity.NewTimelineWorkspaceStore(sdkworkspace.Sub(ws, "views/entity_timeline")),
		Index:               index,
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

func plannedStageNamed(stages []memory.PlannedStage, name string) bool {
	for _, stage := range stages {
		if stage.Name == name {
			return true
		}
	}
	return false
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

func assertDiagnosticCheck(t *testing.T, report memory.DiagnosticReport, name string, status memory.DiagnosticStatus, ok bool) memory.DiagnosticCheck {
	t.Helper()
	return assertDiagnosticCheckIn(t, report.Checks, name, status, ok)
}

func assertFreshnessCheck(t *testing.T, result memory.FreshnessResult, name string, status memory.DiagnosticStatus, ok bool) memory.DiagnosticCheck {
	t.Helper()
	return assertDiagnosticCheckIn(t, result.Checks, name, status, ok)
}

func assertDiagnosticCheckIn(t *testing.T, checks []memory.DiagnosticCheck, name string, status memory.DiagnosticStatus, ok bool) memory.DiagnosticCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			if check.Status != status || check.OK != ok {
				t.Fatalf("Diagnostic check %q = status %q ok %v, want status %q ok %v; check=%+v", name, check.Status, check.OK, status, ok, check)
			}
			return check
		}
	}
	t.Fatalf("Diagnostic check %q missing from checks %+v", name, checks)
	return memory.DiagnosticCheck{}
}

func diagnosticDetailString(t *testing.T, check memory.DiagnosticCheck, key string) string {
	t.Helper()
	got, ok := check.Details[key].(string)
	if !ok || got == "" {
		t.Fatalf("Diagnostic check %q details[%q] = %#v, want non-empty string", check.Name, key, check.Details[key])
	}
	return got
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

func documentScope(datasetID string) memory.Scope {
	scope := testScope("conv-doc")
	scope.DatasetID = datasetID
	return scope
}

func scopedProjectionNamespace(t *testing.T, mem *memory.System, capability memory.Capability, scope memory.Scope) string {
	t.Helper()
	base, ok := mem.ProjectionNamespace(capability)
	if !ok {
		t.Fatalf("ProjectionNamespace(%q) ok = false", capability)
	}
	namespace, err := projectors.ScopedNamespace(base, scope)
	if err != nil {
		t.Fatalf("ScopedNamespace(%q, %+v) error = %v", base, scope, err)
	}
	return namespace
}

type fakeDiagnosticProbe struct {
	calls int
	stage string
	scope memory.Scope
	trace memory.TraceID
}

func (f *fakeDiagnosticProbe) Run(_ context.Context, req memory.DiagnosticProbeRequest) (memory.DiagnosticProbeResult, error) {
	f.calls++
	f.stage = req.Stage
	f.scope = req.Scope
	f.trace = req.TraceID
	return memory.DiagnosticProbeResult{
		Checks: []memory.DiagnosticCheck{{
			Name:     "diagnostics.probe.custom",
			Status:   memory.DiagnosticStatusOK,
			OK:       true,
			Severity: memory.DiagnosticSeverityInfo,
			Message:  "custom probe ran",
			Details: map[string]any{
				"stage": req.Stage,
			},
		}},
	}, nil
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

func entityRetrievalSpec() memory.Spec {
	return memory.Spec{
		Sources: []memory.SourceSpec{{Kind: memory.SourceMessageLog, Required: true}},
		Capabilities: []memory.CapabilitySpec{
			{Capability: memory.CapabilityRecentWindow, Required: true},
			{Capability: memory.CapabilityObservationLedger, Required: true},
			{Capability: memory.CapabilityFactLedger, Required: true},
			{Capability: memory.CapabilityFactGraph, Required: true},
			{Capability: memory.CapabilityEntityProfile, Required: true},
			{Capability: memory.CapabilityEntityTimeline, Required: true},
		},
		Projections: []memory.ProjectionSpec{
			{Capability: memory.CapabilityObservationLedger, Namespace: "observations", Required: true},
			{Capability: memory.CapabilityFactLedger, Namespace: "facts", Required: true},
			{Capability: memory.CapabilityFactGraph, Namespace: "fact_graph", Required: true},
			{Capability: memory.CapabilityEntityProfile, Namespace: "entity_profiles", Required: true},
			{Capability: memory.CapabilityEntityTimeline, Namespace: "entity_timeline", Required: true},
		},
		WriteStages: []memory.StageSpec{
			{Name: "append_message"},
			{Name: "extract_observations"},
			{Name: "reconcile_facts"},
			{Name: "build_fact_graph"},
			{Name: "build_entity_profiles"},
			{Name: "build_entity_timeline"},
		},
		ReadStages: []memory.StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_observations"},
			{Name: "retrieve_facts"},
			{Name: "retrieve_fact_graph"},
			{Name: "retrieve_entity_profiles"},
			{Name: "retrieve_entity_timeline"},
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

type fakeMemoryContextPacker struct {
	calls int
	input derive.ContextPackInput
	fn    func(derive.ContextPackInput) (derive.ContextPackOutput, error)
}

func (f *fakeDocumentChunker) ChunkDocument(_ context.Context, input derive.DocumentChunkInput) ([]viewdocument.Chunk, error) {
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

func (f *fakeMemoryContextPacker) PackContext(_ context.Context, input derive.ContextPackInput) (derive.ContextPackOutput, error) {
	f.calls++
	f.input = input
	if f.fn != nil {
		return f.fn(input)
	}
	return derive.ContextPackOutput{Items: input.Items}, nil
}

func (f *fakeSummarizer) Summarize(_ context.Context, input derive.SummaryInput) ([]recent.SummaryNode, error) {
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

func (f *fakeObservationExtractor) ExtractObservations(_ context.Context, input derive.ObservationInput) ([]viewobservation.Observation, error) {
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
	calls     int
	lastInput derive.FactReconcileInput
	statuses  []fact.FactStatus
}

func (f *fakeFactReconciler) ReconcileFacts(_ context.Context, input derive.FactReconcileInput) ([]fact.Fact, error) {
	f.calls++
	f.lastInput = input
	if len(input.Observations) == 0 {
		return nil, nil
	}
	obs := input.Observations[0]
	statuses := f.statuses
	if len(statuses) == 0 {
		statuses = []fact.FactStatus{fact.FactActive}
	}
	facts := make([]fact.Fact, 0, len(statuses))
	baseID := fact.FactID(scopedID("fact", obs.Scope))
	for i, status := range statuses {
		id := baseID
		if i > 0 {
			id = fact.FactID(string(baseID) + "-" + string(status))
		}
		facts = append(facts, fact.Fact{
			ID:         id,
			Scope:      obs.Scope,
			Subject:    obs.Subject,
			Predicate:  obs.Predicate,
			Object:     obs.Object,
			Status:     status,
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
		})
	}
	return facts, nil
}

type lifecycleFactReconciler struct{}

func (l lifecycleFactReconciler) ReconcileFacts(_ context.Context, input derive.FactReconcileInput) ([]fact.Fact, error) {
	if len(input.Observations) == 0 {
		return nil, nil
	}
	obs := input.Observations[0]
	statuses := []struct {
		id     fact.FactID
		status fact.FactStatus
	}{
		{id: "fact-active", status: fact.FactActive},
		{id: "fact-retracted", status: fact.FactRetracted},
		{id: "fact-superseded", status: fact.FactSuperseded},
		{id: "fact-conflict", status: fact.FactConflict},
	}
	out := make([]fact.Fact, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, fact.Fact{
			ID:         status.id,
			Scope:      obs.Scope,
			Subject:    obs.Subject,
			Predicate:  obs.Predicate,
			Object:     obs.Object,
			Status:     status.status,
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
				TransformSignature: "lifecycle-fact:v1",
			},
		})
	}
	return out, nil
}

type fakeFactGraphBuilder struct {
	calls int
}

func (f *fakeFactGraphBuilder) BuildFactGraph(_ context.Context, input derive.FactGraphInput) (derive.FactGraphOutput, error) {
	f.calls++
	if len(input.Facts) == 0 {
		return derive.FactGraphOutput{}, nil
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
	return derive.FactGraphOutput{
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

type fakeEntityProfileBuilder struct {
	calls int
}

func (f *fakeEntityProfileBuilder) BuildEntityProfiles(_ context.Context, input derive.EntityProfileInput) ([]viewentity.ProfileRecord, error) {
	f.calls++
	if len(input.Facts) == 0 {
		return nil, nil
	}
	record := input.Facts[0]
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	return []viewentity.ProfileRecord{{
		ID:         viewentity.ProfileID("profile-" + safeIDPart(input.Scope.EntityID)),
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
	calls int
}

func (f *fakeEntityTimelineBuilder) BuildEntityTimeline(_ context.Context, input derive.EntityTimelineInput) ([]viewentity.Event, error) {
	f.calls++
	if len(input.Facts) == 0 {
		return nil, nil
	}
	record := input.Facts[0]
	factRefs := []fact.FactRef{{FactID: record.ID, Role: "supporting_fact"}}
	return []viewentity.Event{{
		ID:          viewentity.EventID("event-" + safeIDPart(input.Scope.EntityID)),
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

func upstreamEntityViewID(graph derive.FactGraphOutput, record fact.Fact) views.ID {
	if len(graph.Nodes) > 0 && graph.Nodes[0].Signature.ViewID != "" {
		return graph.Nodes[0].Signature.ViewID
	}
	return record.Signature.ViewID
}

func upstreamEntitySignature(graph derive.FactGraphOutput, record fact.Fact) string {
	if len(graph.Nodes) > 0 && graph.Nodes[0].Signature.TransformSignature != "" {
		return graph.Nodes[0].Signature.TransformSignature
	}
	return record.Signature.TransformSignature
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
	parts := []string{prefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, scope.EntityID}
	for i, part := range parts {
		if part == "" {
			parts[i] = "global"
			continue
		}
		parts[i] = safeIDPart(part)
	}
	return strings.Join(parts, "-")
}

func safeIDPart(part string) string {
	return strings.NewReplacer(":", "-", "/", "-", " ", "-").Replace(part)
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
