package compiler

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/internal/projectors"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

func TestCompileSupportedCapabilities(t *testing.T) {
	assembly := requireCompile(t, Spec{
		Sources: []SourceSpec{
			{Kind: SourceMessageLog, Required: true},
			{Kind: SourceDocumentStore, Required: true},
		},
		Capabilities: []CapabilitySpec{
			{Capability: CapabilityRecentWindow, Required: true, Purpose: "recent"},
			{Capability: CapabilityMessageIndex, Required: true},
			{Capability: CapabilitySummaryDAG, Required: true},
			{Capability: CapabilityDocumentChunks, Required: true},
		},
		Projections: []ProjectionRequest{
			{Capability: CapabilityMessageIndex, Namespace: "message_index", Required: true},
			{Capability: CapabilitySummaryDAG, Namespace: "summary_nodes", Required: true},
			{Capability: CapabilityDocumentChunks, Namespace: "document_chunks", Required: true},
		},
		WriteStages: []StageSpec{{Name: "append_message"}, {Name: "index_messages"}, {Name: "build_summary_dag"}},
		ReadStages:  []StageSpec{{Name: "load_recent_messages"}, {Name: "retrieve_messages"}, {Name: "retrieve_summaries"}, {Name: "retrieve_documents"}, {Name: "pack_context"}},
	})

	requireView(t, assembly, CapabilityRecentWindow, views.Descriptor{ID: recent.DefaultWindowID, Kind: views.KindRecentWindow, Version: recent.DefaultWindowVersion}, true, "recent")
	requireView(t, assembly, CapabilityMessageIndex, views.Descriptor{ID: views.ID("message_index"), Kind: views.KindMessageIndex, Version: "v1"}, true, "")
	requireView(t, assembly, CapabilitySummaryDAG, views.Descriptor{ID: recent.DefaultSummaryDAGID, Kind: views.KindSummaryDAG, Version: recent.DefaultSummaryDAGVersion}, true, "")
	requireView(t, assembly, CapabilityDocumentChunks, views.Descriptor{ID: document.DefaultChunksID, Kind: views.KindDocumentChunks, Version: document.DefaultChunksVersion}, true, "")
	requireProjection(t, assembly, "message_index", CapabilityMessageIndex, []string{projectors.RecordTypeSourceMessage}, views.KindMessageIndex, []string{"SourceMessageRecords"})
	requireProjection(t, assembly, "summary_nodes", CapabilitySummaryDAG, []string{projectors.RecordTypeSummaryNode}, views.KindSummaryDAG, []string{"SummaryNode"})
	requireProjection(t, assembly, "document_chunks", CapabilityDocumentChunks, []string{projectors.RecordTypeDocumentChunk}, views.KindDocumentChunks, []string{"DocumentChunk"})
}

func TestCompileRejectsRemovedCapabilities(t *testing.T) {
	for _, capability := range []Capability{
		"observation_ledger",
		"fact_ledger",
		"fact_graph",
		"entity_profile",
		"entity_timeline",
	} {
		t.Run(string(capability), func(t *testing.T) {
			_, err := Compile(Spec{
				Sources:      []SourceSpec{{Kind: SourceMessageLog}},
				Capabilities: []CapabilitySpec{{Capability: capability}},
			})
			if err == nil {
				t.Fatalf("Compile() error = nil, want removed capability rejected")
			}
		})
	}
}

func TestCompileRejectsProjectionForRemovedCapability(t *testing.T) {
	_, err := Compile(Spec{
		Sources:      []SourceSpec{{Kind: SourceMessageLog}},
		Capabilities: []CapabilitySpec{{Capability: CapabilityRecentWindow}},
		Projections: []ProjectionRequest{
			{Capability: "fact_ledger", Namespace: "facts"},
		},
	})
	if err == nil {
		t.Fatalf("Compile() error = nil, want removed projection capability rejected")
	}
}

func TestValidateRejectsInvalidSpecs(t *testing.T) {
	tests := map[string]Spec{
		"unknown source": {
			Sources: []SourceSpec{{Kind: "unknown"}},
		},
		"duplicate source": {
			Sources: []SourceSpec{{Kind: SourceMessageLog}, {Kind: SourceMessageLog}},
		},
		"unknown capability": {
			Sources:      []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{{Capability: "unknown"}},
		},
		"duplicate capability": {
			Sources:      []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{{Capability: CapabilityRecentWindow}, {Capability: CapabilityRecentWindow}},
		},
		"recent window without message source": {
			Sources:      []SourceSpec{{Kind: SourceDocumentStore}},
			Capabilities: []CapabilitySpec{{Capability: CapabilityRecentWindow}},
		},
		"document chunks without document source": {
			Sources:      []SourceSpec{{Kind: SourceMessageLog}},
			Capabilities: []CapabilitySpec{{Capability: CapabilityDocumentChunks}},
		},
		"summary without message source": {
			Sources:      []SourceSpec{{Kind: SourceDocumentStore}},
			Capabilities: []CapabilitySpec{{Capability: CapabilitySummaryDAG}},
		},
		"message index without message source": {
			Sources:      []SourceSpec{{Kind: SourceDocumentStore}},
			Capabilities: []CapabilitySpec{{Capability: CapabilityMessageIndex}},
		},
		"duplicate stage": {
			Sources:     []SourceSpec{{Kind: SourceMessageLog}},
			ReadStages:  []StageSpec{{Name: "pack_context"}, {Name: "pack_context"}},
			Diagnostics: []StageSpec{{Name: "freshness"}},
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

func requireView(t *testing.T, assembly Assembly, capability Capability, descriptor views.Descriptor, required bool, purpose string) {
	t.Helper()
	for _, view := range assembly.Views {
		if view.Capability != capability {
			continue
		}
		if view.Descriptor != descriptor || view.Required != required || view.Purpose != purpose {
			t.Fatalf("view %q = %+v, want descriptor=%+v required=%v purpose=%q", capability, view, descriptor, required, purpose)
		}
		return
	}
	t.Fatalf("missing view capability %q", capability)
}

func requireProjection(t *testing.T, assembly Assembly, namespace string, capability Capability, recordTypes []string, kind views.Kind, projectors []string) {
	t.Helper()
	for _, projection := range assembly.Projections {
		if projection.Binding.Namespace != namespace {
			continue
		}
		if projection.Capability != capability || projection.ViewKind != kind ||
			!reflect.DeepEqual(projection.RecordTypes, recordTypes) ||
			!reflect.DeepEqual(projection.Projectors, projectors) {
			t.Fatalf("projection %q = %+v", namespace, projection)
		}
		return
	}
	t.Fatalf("missing projection namespace %q", namespace)
}
