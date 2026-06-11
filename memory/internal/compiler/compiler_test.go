package compiler

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

const (
	nsSummaryNodes   = "summary_nodes"
	nsDocumentChunks = "document_chunks"
	nsObservations   = "observations"
	nsFacts          = "facts"
	nsFactGraph      = "fact_graph"
	nsEntityProfiles = "entity_profiles"
	nsEntityEvents   = "entity_events"
)

func TestCompileConversationLikeSpec(t *testing.T) {
	assembly := requireCompile(t, Spec{
		Sources: []SourceSpec{
			{Kind: SourceMessageLog, Required: true},
		},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityRecentWindow, Required: true, Purpose: "recent message context"},
		},
		WriteStages: []StageSpec{
			{Name: "append_message"},
		},
		ReadStages: []StageSpec{
			{Name: "load_recent_messages"},
			{Name: "pack_context"},
		},
	})

	if len(assembly.Sources) != 1 {
		t.Fatalf("Sources len = %d, want 1", len(assembly.Sources))
	}
	if assembly.Sources[0] != (SourceSpec{Kind: SourceMessageLog, Required: true}) {
		t.Fatalf("Sources[0] = %+v, want required message log", assembly.Sources[0])
	}
	if len(assembly.Views) != 1 {
		t.Fatalf("Views len = %d, want 1", len(assembly.Views))
	}
	requireView(t, viewsByCapability(assembly), CapabilityRecentWindow, views.Descriptor{
		ID:      recent.DefaultWindowID,
		Kind:    views.KindRecentWindow,
		Version: recent.DefaultWindowVersion,
	}, true, "recent message context")
	if len(assembly.Projections) != 0 {
		t.Fatalf("Projections len = %d, want 0", len(assembly.Projections))
	}
	requireStageNames(t, assembly.WriteStages, "append_message")
	requireStageNames(t, assembly.ReadStages, "load_recent_messages", "pack_context")
}

func TestCompileLongMemoryCapabilitySpec(t *testing.T) {
	assembly := requireCompile(t, Spec{
		Sources: []SourceSpec{
			{Kind: SourceMessageLog, Required: true},
		},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityRecentWindow, Required: true},
			{Capability: CapabilitySummaryDAG, Required: true},
			{Capability: CapabilityObservationLedger, Required: true},
			{Capability: CapabilityFactLedger, Required: true},
			{Capability: CapabilityFactGraph, Required: true},
			{Capability: CapabilityEntityProfile, Required: false},
			{Capability: CapabilityEntityTimeline, Required: false},
		},
		Projections: []ProjectionRequest{
			{Capability: CapabilitySummaryDAG, Namespace: nsSummaryNodes, Required: true},
			{Capability: CapabilityObservationLedger, Namespace: nsObservations, Required: true},
			{Capability: CapabilityFactLedger, Namespace: nsFacts, Required: true},
			{Capability: CapabilityFactGraph, Namespace: nsFactGraph, Required: true},
			{Capability: CapabilityEntityProfile, Namespace: nsEntityProfiles, Required: false},
			{Capability: CapabilityEntityTimeline, Namespace: nsEntityEvents, Required: false},
		},
		WriteStages: []StageSpec{
			{Name: "append_message"},
			{Name: "extract_facts", Async: true},
			{Name: "ground_evidence", Async: true},
			{Name: "update_fact_ledger", Async: true},
			{Name: "update_fact_views", Async: true},
			{Name: "update_index", Async: true},
		},
		ReadStages: []StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_relevant_summaries"},
			{Name: "retrieve_recall_facts"},
			{Name: "expand_fact_graph"},
			{Name: "rerank"},
			{Name: "pack_context"},
		},
		Lifecycle: []StageSpec{
			{Name: "compact"},
			{Name: "extract_facts", Async: true},
			{Name: "reconcile"},
			{Name: "shutdown"},
		},
		Diagnostics: []StageSpec{
			{Name: "trace"},
			{Name: "freshness"},
			{Name: "readiness"},
			{Name: "queue_stats"},
		},
	})

	viewMap := viewsByCapability(assembly)
	requireView(t, viewMap, CapabilityRecentWindow, descriptor(recent.DefaultWindowID, views.KindRecentWindow, recent.DefaultWindowVersion), true, "")
	requireView(t, viewMap, CapabilitySummaryDAG, descriptor(recent.DefaultSummaryDAGID, views.KindSummaryDAG, recent.DefaultSummaryDAGVersion), true, "")
	requireView(t, viewMap, CapabilityObservationLedger, descriptor(observation.DefaultLedgerID, views.KindObservationLedger, observation.DefaultLedgerVersion), true, "")
	requireView(t, viewMap, CapabilityFactLedger, descriptor(fact.DefaultLedgerID, views.KindFactLedger, fact.DefaultLedgerVersion), true, "")
	requireView(t, viewMap, CapabilityFactGraph, descriptor(fact.DefaultGraphID, views.KindFactGraph, fact.DefaultGraphVersion), true, "")
	requireView(t, viewMap, CapabilityEntityProfile, descriptor(entity.DefaultProfileID, views.KindEntityProfile, entity.DefaultProfileVersion), false, "")
	requireView(t, viewMap, CapabilityEntityTimeline, descriptor(entity.DefaultTimelineID, views.KindEntityTimeline, entity.DefaultTimelineVersion), false, "")

	projections := projectionsByNamespace(assembly)
	requireProjection(t, projections, nsSummaryNodes, CapabilitySummaryDAG, []string{projectors.RecordTypeSummaryNode}, views.KindSummaryDAG, []string{"SummaryNode"}, true)
	requireProjection(t, projections, nsObservations, CapabilityObservationLedger, []string{projectors.RecordTypeObservation}, views.KindObservationLedger, []string{"Observation"}, true)
	requireProjection(t, projections, nsFacts, CapabilityFactLedger, []string{projectors.RecordTypeFact}, views.KindFactLedger, []string{"FactRecord"}, true)
	requireProjection(t, projections, nsFactGraph, CapabilityFactGraph, []string{projectors.RecordTypeFactNode, projectors.RecordTypeFactEdge}, views.KindFactGraph, []string{"FactNode", "FactEdge"}, true)
	requireProjection(t, projections, nsEntityProfiles, CapabilityEntityProfile, []string{projectors.RecordTypeEntityProfile}, views.KindEntityProfile, []string{"EntityProfile"}, false)
	requireProjection(t, projections, nsEntityEvents, CapabilityEntityTimeline, []string{projectors.RecordTypeEntityEvent}, views.KindEntityTimeline, []string{"EntityEvent"}, false)

	requireAsyncStages(t, assembly.WriteStages,
		"extract_facts",
		"ground_evidence",
		"update_fact_ledger",
		"update_fact_views",
		"update_index",
	)
	requireStageNames(t, assembly.Diagnostics, "trace", "freshness", "readiness", "queue_stats")
}

func TestCompileDocumentChunksProjectionSpec(t *testing.T) {
	assembly := requireCompile(t, Spec{
		Sources: []SourceSpec{
			{Kind: SourceMessageLog, Required: true},
			{Kind: SourceDocumentStore, Required: true},
		},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityRecentWindow, Required: true},
			{Capability: CapabilityDocumentChunks, Required: true, Purpose: "document chunk recall"},
		},
		Projections: []ProjectionRequest{
			{Capability: CapabilityDocumentChunks, Namespace: nsDocumentChunks, Required: true},
		},
		WriteStages: []StageSpec{
			{Name: "append_message"},
			{Name: "put_document"},
			{Name: "chunk_document"},
			{Name: "build_document_index"},
		},
		ReadStages: []StageSpec{
			{Name: "load_recent_messages"},
			{Name: "retrieve_documents"},
			{Name: "pack_context"},
		},
	})

	requireSourceKinds(t, assembly, SourceMessageLog, SourceDocumentStore)
	requireView(t, viewsByCapability(assembly), CapabilityDocumentChunks, descriptor(document.DefaultChunksID, views.KindDocumentChunks, document.DefaultChunksVersion), true, "document chunk recall")
	requireProjection(t, projectionsByNamespace(assembly), nsDocumentChunks, CapabilityDocumentChunks, []string{projectors.RecordTypeDocumentChunk}, views.KindDocumentChunks, []string{"DocumentChunk"}, true)
}

func TestCompileFactGraphProjectionUsesSeparateArrays(t *testing.T) {
	assembly := requireCompile(t, Spec{
		Sources: []SourceSpec{
			{Kind: SourceMessageLog, Required: true},
		},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityObservationLedger, Required: true},
			{Capability: CapabilityFactLedger, Required: true},
			{Capability: CapabilityFactGraph, Required: true},
		},
		Projections: []ProjectionRequest{
			{Capability: CapabilityFactGraph, Namespace: nsFactGraph, Required: true},
		},
	})

	projection := projectionsByNamespace(assembly)[nsFactGraph]
	if !reflect.DeepEqual(projection.RecordTypes, []string{projectors.RecordTypeFactNode, projectors.RecordTypeFactEdge}) {
		t.Fatalf("RecordTypes = %#v, want separate fact node/edge record types", projection.RecordTypes)
	}
	if !reflect.DeepEqual(projection.Projectors, []string{"FactNode", "FactEdge"}) {
		t.Fatalf("Projectors = %#v, want separate fact node/edge projectors", projection.Projectors)
	}
	if projection.Binding.Namespace != nsFactGraph {
		t.Fatalf("Binding.Namespace = %q, want %q", projection.Binding.Namespace, nsFactGraph)
	}
}

func TestValidateRejectsInvalidSpecs(t *testing.T) {
	tests := map[string]Spec{
		"unknown source": {
			Sources: []SourceSpec{{Kind: "unknown"}},
		},
		"duplicate source": {
			Sources: []SourceSpec{
				{Kind: SourceMessageLog},
				{Kind: SourceMessageLog},
			},
		},
		"unknown capability": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: "unknown"},
			},
		},
		"duplicate capability": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityRecentWindow},
				{Capability: CapabilityRecentWindow},
			},
		},
		"projection for disabled capability": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityRecentWindow},
			},
			Projections: []ProjectionRequest{
				{Capability: CapabilitySummaryDAG, Namespace: nsSummaryNodes},
			},
		},
		"invalid namespace": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilitySummaryDAG},
			},
			Projections: []ProjectionRequest{
				{Capability: CapabilitySummaryDAG, Namespace: "bad-name"},
			},
		},
		"duplicate namespace": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilitySummaryDAG},
				{Capability: CapabilityObservationLedger},
				{Capability: CapabilityFactLedger},
			},
			Projections: []ProjectionRequest{
				{Capability: CapabilitySummaryDAG, Namespace: nsFacts},
				{Capability: CapabilityFactLedger, Namespace: nsFacts},
			},
		},
		"recent window without message source": {
			Sources: []SourceSpec{{Kind: SourceDocumentStore}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityRecentWindow},
			},
		},
		"document chunks without document source": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityDocumentChunks},
			},
		},
		"summary without message source": {
			Sources: []SourceSpec{{Kind: SourceDocumentStore}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilitySummaryDAG},
			},
		},
		"fact ledger without observation ledger": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityFactLedger},
			},
		},
		"fact graph without fact ledger": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityObservationLedger},
				{Capability: CapabilityFactGraph},
			},
		},
		"entity profile without fact graph": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityObservationLedger},
				{Capability: CapabilityFactLedger},
				{Capability: CapabilityEntityProfile},
			},
		},
		"entity timeline without fact graph": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{
				{Capability: CapabilityObservationLedger},
				{Capability: CapabilityFactLedger},
				{Capability: CapabilityEntityTimeline},
			},
		},
		"missing stage name": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			WriteStages: []StageSpec{
				{Name: ""},
			},
		},
		"duplicate stage in one list": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}},
			ReadStages: []StageSpec{
				{Name: "pack_context"},
				{Name: "pack_context"},
			},
		},
	}

	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if err := spec.Validate(); err == nil {
				t.Fatalf("Spec.Validate() error = nil, want error")
			}
		})
	}
}

func TestAssemblyValidateRejectsDuplicateViewID(t *testing.T) {
	assembly := Assembly{
		Sources: []SourceSpec{{Kind: SourceMessageLog}},
		Views: []ViewAssembly{
			{
				Capability: CapabilityRecentWindow,
				Descriptor: descriptor(recent.DefaultWindowID, views.KindRecentWindow, recent.DefaultWindowVersion),
			},
			{
				Capability: CapabilitySummaryDAG,
				Descriptor: descriptor(recent.DefaultWindowID, views.KindSummaryDAG, recent.DefaultSummaryDAGVersion),
			},
		},
	}

	if err := assembly.Validate(); err == nil {
		t.Fatalf("Validate() error = nil, want duplicate view id error")
	}
}

func TestProjectionNamespaceValidationUsesIndexedBinding(t *testing.T) {
	if err := (indexed.Binding{Namespace: "bad-name"}).Validate(); err == nil {
		t.Fatalf("indexed.Binding.Validate() error = nil, want invalid namespace error")
	}

	_, err := Compile(Spec{
		Sources: []SourceSpec{{Kind: SourceMessageLog}},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityObservationLedger},
			{Capability: CapabilityFactLedger},
		},
		Projections: []ProjectionRequest{
			{Capability: CapabilityFactLedger, Namespace: "bad-name"},
		},
	})
	if err == nil {
		t.Fatalf("Compile() error = nil, want invalid namespace error")
	}
}

func requireCompile(t *testing.T, spec Spec) Assembly {
	t.Helper()
	assembly, err := Compile(spec)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if err := assembly.Validate(); err != nil {
		t.Fatalf("Assembly.Validate() error = %v", err)
	}
	return assembly
}

func descriptor(id views.ID, kind views.Kind, version string) views.Descriptor {
	return views.Descriptor{
		ID:      id,
		Kind:    kind,
		Version: version,
	}
}

func requireSourceKinds(t *testing.T, assembly Assembly, want ...SourceKind) {
	t.Helper()
	if len(assembly.Sources) != len(want) {
		t.Fatalf("Sources len = %d, want %d", len(assembly.Sources), len(want))
	}
	for i, kind := range want {
		if assembly.Sources[i].Kind != kind {
			t.Fatalf("Sources[%d].Kind = %q, want %q", i, assembly.Sources[i].Kind, kind)
		}
		if !assembly.Sources[i].Required {
			t.Fatalf("Sources[%d].Required = false, want true", i)
		}
	}
}

func requireView(t *testing.T, viewMap map[Capability]ViewAssembly, capability Capability, descriptor views.Descriptor, required bool, purpose string) {
	t.Helper()
	view, ok := viewMap[capability]
	if !ok {
		t.Fatalf("missing view capability %q", capability)
	}
	if view.Descriptor != descriptor {
		t.Fatalf("view %q descriptor = %+v, want %+v", capability, view.Descriptor, descriptor)
	}
	if view.Required != required {
		t.Fatalf("view %q Required = %v, want %v", capability, view.Required, required)
	}
	if view.Purpose != purpose {
		t.Fatalf("view %q Purpose = %q, want %q", capability, view.Purpose, purpose)
	}
}

func requireProjection(t *testing.T, projections map[string]ProjectionAssembly, namespace string, capability Capability, recordTypes []string, kind views.Kind, projectors []string, required bool) {
	t.Helper()
	projection, ok := projections[namespace]
	if !ok {
		t.Fatalf("missing projection namespace %q", namespace)
	}
	if projection.Capability != capability {
		t.Fatalf("projection %q Capability = %q, want %q", namespace, projection.Capability, capability)
	}
	if !reflect.DeepEqual(projection.RecordTypes, recordTypes) {
		t.Fatalf("projection %q RecordTypes = %#v, want %#v", namespace, projection.RecordTypes, recordTypes)
	}
	if projection.ViewKind != kind {
		t.Fatalf("projection %q ViewKind = %q, want %q", namespace, projection.ViewKind, kind)
	}
	if projection.Binding.Namespace != namespace {
		t.Fatalf("projection %q Binding.Namespace = %q, want %q", namespace, projection.Binding.Namespace, namespace)
	}
	if !reflect.DeepEqual(projection.Projectors, projectors) {
		t.Fatalf("projection %q Projectors = %#v, want %#v", namespace, projection.Projectors, projectors)
	}
	if projection.Required != required {
		t.Fatalf("projection %q Required = %v, want %v", namespace, projection.Required, required)
	}
}

func requireStageNames(t *testing.T, stages []StageSpec, want ...string) {
	t.Helper()
	if len(stages) != len(want) {
		t.Fatalf("stages len = %d, want %d", len(stages), len(want))
	}
	for i, name := range want {
		if stages[i].Name != name {
			t.Fatalf("stages[%d].Name = %q, want %q", i, stages[i].Name, name)
		}
	}
}

func requireAsyncStages(t *testing.T, stages []StageSpec, want ...string) {
	t.Helper()
	byName := make(map[string]StageSpec, len(stages))
	for _, stage := range stages {
		byName[stage.Name] = stage
	}
	for _, name := range want {
		stage, ok := byName[name]
		if !ok {
			t.Fatalf("missing async stage %q", name)
		}
		if !stage.Async {
			t.Fatalf("stage %q Async = false, want true", name)
		}
	}
}

func viewsByCapability(assembly Assembly) map[Capability]ViewAssembly {
	out := make(map[Capability]ViewAssembly, len(assembly.Views))
	for _, view := range assembly.Views {
		out[view.Capability] = view
	}
	return out
}

func projectionsByNamespace(assembly Assembly) map[string]ProjectionAssembly {
	out := make(map[string]ProjectionAssembly, len(assembly.Projections))
	for _, projection := range assembly.Projections {
		out[projection.Binding.Namespace] = projection
	}
	return out
}
