package projectors

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
)

func TestProjectorsReturnValidIndexedRecords(t *testing.T) {
	tests := []struct {
		name            string
		project         func() (indexed.Record, error)
		wantID          string
		wantViewKind    views.Kind
		wantRecordType  string
		wantSignatureID views.ID
		wantText        []string
	}{
		{
			name:            "document chunk",
			project:         func() (indexed.Record, error) { return DocumentChunk(validChunk()) },
			wantID:          "document_chunk:ZGF0YXNldC0x:ZG9jLTE:Y2h1bmstMQ",
			wantViewKind:    views.KindDocumentChunks,
			wantRecordType:  RecordTypeDocumentChunk,
			wantSignatureID: "document-chunks",
			wantText:        []string{"chunk text"},
		},
		{
			name:            "summary node",
			project:         func() (indexed.Record, error) { return SummaryNode(validSummaryNode()) },
			wantID:          "summary_node:Y29udi0x:c3VtbWFyeS0x",
			wantViewKind:    views.KindSummaryDAG,
			wantRecordType:  RecordTypeSummaryNode,
			wantSignatureID: "summary-dag",
			wantText:        []string{"summary text"},
		},
		{
			name:            "observation",
			project:         func() (indexed.Record, error) { return Observation(validObservation()) },
			wantID:          "observation:obs-1",
			wantViewKind:    views.KindObservationLedger,
			wantRecordType:  RecordTypeObservation,
			wantSignatureID: "observation-ledger",
			wantText:        []string{"Observation: user:1 likes tea", "Scope: runtime=runtime-1 user=user-1 agent=agent-1", "Confidence: 0.7"},
		},
		{
			name:            "fact record",
			project:         func() (indexed.Record, error) { return FactRecord(validFact()) },
			wantID:          "fact:fact-1",
			wantViewKind:    views.KindFactLedger,
			wantRecordType:  RecordTypeFact,
			wantSignatureID: "fact-ledger",
			wantText:        []string{"Fact: user:1 likes tea", "Status: active", "Confidence: 0.8", "ValidFrom:"},
		},
		{
			name:            "fact node",
			project:         func() (indexed.Record, error) { return FactNode(validFactNode()) },
			wantID:          "fact_node:entity:user-1",
			wantViewKind:    views.KindFactGraph,
			wantRecordType:  RecordTypeFactNode,
			wantSignatureID: "fact-graph",
			wantText:        []string{"Node: User One", "Kind: entity", "Aliases: U1, User 1"},
		},
		{
			name:            "fact edge",
			project:         func() (indexed.Record, error) { return FactEdge(validFactEdge()) },
			wantID:          "fact_edge:edge-1",
			wantViewKind:    views.KindFactGraph,
			wantRecordType:  RecordTypeFactEdge,
			wantSignatureID: "fact-graph",
			wantText:        []string{"Edge: entity:user-1 likes value:tea", "Status: active", "Confidence: 0.9", "ValidUntil:"},
		},
		{
			name:            "entity profile",
			project:         func() (indexed.Record, error) { return EntityProfile(validProfile()) },
			wantID:          "entity_profile:profile-1",
			wantViewKind:    views.KindEntityProfile,
			wantRecordType:  RecordTypeEntityProfile,
			wantSignatureID: "entity-profile",
			wantText:        []string{"Profile: User One", "Summary: likes tea", "favorite_drink: tea"},
		},
		{
			name:            "entity event",
			project:         func() (indexed.Record, error) { return EntityEvent(validEvent()) },
			wantID:          "entity_event:event-1",
			wantViewKind:    views.KindEntityTimeline,
			wantRecordType:  RecordTypeEntityEvent,
			wantSignatureID: "entity-timeline",
			wantText:        []string{"Event: Tried tea", "Description: user tried tea", "OccurredAt:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record, err := tt.project()
			if err != nil {
				t.Fatalf("project() error = %v", err)
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("indexed record Validate() error = %v", err)
			}
			if record.ID != tt.wantID {
				t.Fatalf("ID = %q, want %q", record.ID, tt.wantID)
			}
			if strings.TrimSpace(record.Text) == "" {
				t.Fatal("Text is empty")
			}
			for _, want := range tt.wantText {
				if !strings.Contains(record.Text, want) {
					t.Fatalf("Text = %q, want substring %q", record.Text, want)
				}
			}
			if len(record.Vector) != 0 {
				t.Fatalf("Vector = %v, want no generated vector", record.Vector)
			}
			if got := record.Metadata[MetadataViewKindKey]; got != string(tt.wantViewKind) {
				t.Fatalf("metadata view_kind = %v, want %q", got, tt.wantViewKind)
			}
			if got := record.Metadata[MetadataRecordTypeKey]; got != tt.wantRecordType {
				t.Fatalf("metadata record_type = %v, want %q", got, tt.wantRecordType)
			}
			if len(record.SourceRefs) != 1 {
				t.Fatalf("SourceRefs len = %d, want 1", len(record.SourceRefs))
			}
			if err := record.SourceRefs[0].Validate(); err != nil {
				t.Fatalf("SourceRefs[0] Validate() error = %v", err)
			}
			if record.Signature.ViewID != tt.wantSignatureID {
				t.Fatalf("Signature.ViewID = %q, want %q", record.Signature.ViewID, tt.wantSignatureID)
			}
		})
	}
}

func TestScopedProjectorsIncludeUnifiedScopeMetadata(t *testing.T) {
	tests := []struct {
		name    string
		project func() (indexed.Record, error)
	}{
		{name: "observation", project: func() (indexed.Record, error) { return Observation(validObservation()) }},
		{name: "fact", project: func() (indexed.Record, error) { return FactRecord(validFact()) }},
		{name: "fact node", project: func() (indexed.Record, error) { return FactNode(validFactNode()) }},
		{name: "fact edge", project: func() (indexed.Record, error) { return FactEdge(validFactEdge()) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record, err := tt.project()
			if err != nil {
				t.Fatalf("project() error = %v", err)
			}
			want := validScope()
			assertMetadata(t, record, MetadataRuntimeIDKey, want.RuntimeID)
			assertMetadata(t, record, MetadataUserIDKey, want.UserID)
			assertMetadata(t, record, MetadataAgentIDKey, want.AgentID)
			assertMetadata(t, record, MetadataConversationIDKey, want.ConversationID)
			assertMetadata(t, record, MetadataDatasetIDKey, want.DatasetID)
			assertMetadata(t, record, MetadataEntityIDKey, want.EntityID)
		})
	}
}

func TestScopedNamespaceBuildsValidPhysicalPartitions(t *testing.T) {
	userScope := validScope()
	userNamespace, err := ScopedNamespace("observations", userScope)
	if err != nil {
		t.Fatalf("ScopedNamespace(user) error = %v", err)
	}
	if err := (indexed.Binding{Namespace: userNamespace}).Validate(); err != nil {
		t.Fatalf("user namespace Validate() error = %v", err)
	}
	if len(userNamespace) > 48 {
		t.Fatalf("user namespace len = %d, want <= 48", len(userNamespace))
	}
	if !strings.HasPrefix(userNamespace, "observations_rt_") {
		t.Fatalf("user namespace = %q, want base prefix", userNamespace)
	}

	globalScope := userScope
	globalScope.UserID = ""
	globalNamespace, err := ScopedNamespace("observations", globalScope)
	if err != nil {
		t.Fatalf("ScopedNamespace(global) error = %v", err)
	}
	if err := (indexed.Binding{Namespace: globalNamespace}).Validate(); err != nil {
		t.Fatalf("global namespace Validate() error = %v", err)
	}
	if userNamespace == globalNamespace {
		t.Fatalf("user and global namespaces matched: %q", userNamespace)
	}
	if !strings.Contains(userNamespace, "_u_") {
		t.Fatalf("user namespace = %q, want user partition marker", userNamespace)
	}
	if !strings.Contains(globalNamespace, "_g_") {
		t.Fatalf("global namespace = %q, want global partition marker", globalNamespace)
	}

	longNamespace, err := ScopedNamespace("very_long_projection_namespace_to_shorten", userScope)
	if err != nil {
		t.Fatalf("ScopedNamespace(long base) error = %v", err)
	}
	if err := (indexed.Binding{Namespace: longNamespace}).Validate(); err != nil {
		t.Fatalf("long namespace Validate() error = %v", err)
	}
	if len(longNamespace) > 48 {
		t.Fatalf("long namespace len = %d, want <= 48", len(longNamespace))
	}
}

func TestScopedNamespaceRequiresValidBaseAndRuntimeScope(t *testing.T) {
	if _, err := ScopedNamespace("bad-name", validScope()); err == nil {
		t.Fatal("ScopedNamespace invalid base error = nil, want validation error")
	}

	scope := validScope()
	scope.RuntimeID = ""
	if _, err := ScopedNamespace("observations", scope); err == nil {
		t.Fatal("ScopedNamespace missing runtime error = nil, want validation error")
	}
}

func TestDocumentChunkRecordIDEncodesCompositeParts(t *testing.T) {
	left := validChunk()
	setChunkIdentity(&left, "a:b", "c", "d")
	right := validChunk()
	setChunkIdentity(&right, "a", "b:c", "d")

	leftRecord, err := DocumentChunk(left)
	if err != nil {
		t.Fatalf("DocumentChunk(left) error = %v", err)
	}
	if err := leftRecord.Validate(); err != nil {
		t.Fatalf("left record Validate() error = %v", err)
	}
	rightRecord, err := DocumentChunk(right)
	if err != nil {
		t.Fatalf("DocumentChunk(right) error = %v", err)
	}
	if err := rightRecord.Validate(); err != nil {
		t.Fatalf("right record Validate() error = %v", err)
	}

	if leftRecord.ID == rightRecord.ID {
		t.Fatalf("record IDs collided: %q", leftRecord.ID)
	}
	if got := leftRecord.Metadata[MetadataDatasetIDKey]; got != "a:b" {
		t.Fatalf("left dataset metadata = %v, want original %q", got, "a:b")
	}
	if got := rightRecord.Metadata[MetadataDocumentIDKey]; got != "b:c" {
		t.Fatalf("right document metadata = %v, want original %q", got, "b:c")
	}
}

func TestSummaryNodeRecordIDEncodesCompositeParts(t *testing.T) {
	left := validSummaryNode()
	setSummaryNodeIdentity(&left, "a:b", "c")
	right := validSummaryNode()
	setSummaryNodeIdentity(&right, "a", "b:c")

	leftRecord, err := SummaryNode(left)
	if err != nil {
		t.Fatalf("SummaryNode(left) error = %v", err)
	}
	if err := leftRecord.Validate(); err != nil {
		t.Fatalf("left record Validate() error = %v", err)
	}
	rightRecord, err := SummaryNode(right)
	if err != nil {
		t.Fatalf("SummaryNode(right) error = %v", err)
	}
	if err := rightRecord.Validate(); err != nil {
		t.Fatalf("right record Validate() error = %v", err)
	}

	if leftRecord.ID == rightRecord.ID {
		t.Fatalf("record IDs collided: %q", leftRecord.ID)
	}
	if got := leftRecord.Metadata[MetadataConversationIDKey]; got != "a:b" {
		t.Fatalf("left conversation metadata = %v, want original %q", got, "a:b")
	}
	if got := rightRecord.Metadata[MetadataNodeIDKey]; got != "b:c" {
		t.Fatalf("right node metadata = %v, want original %q", got, "b:c")
	}
}

func TestProjectorMetadataDeepCloneAndReservedSourceKeys(t *testing.T) {
	obs := validObservation()
	obs.Metadata = map[string]any{
		indexed.MetadataSourceRefsKey: "nested reserved key is allowed",
		"nested": map[string]any{
			"k": "v",
		},
		"list": []any{
			map[string]any{"item": "v"},
		},
	}

	record, err := Observation(obs)
	if err != nil {
		t.Fatalf("Observation() error = %v", err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Record.Validate() error = %v", err)
	}
	if _, ok := record.Metadata[indexed.MetadataSourceRefsKey]; ok {
		t.Fatalf("top-level metadata contains reserved key %q", indexed.MetadataSourceRefsKey)
	}
	if _, ok := record.Metadata[indexed.MetadataSignatureKey]; ok {
		t.Fatalf("top-level metadata contains reserved key %q", indexed.MetadataSignatureKey)
	}

	copied, ok := record.Metadata[MetadataRecordMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("record metadata = %T, want map[string]any", record.Metadata[MetadataRecordMetadataKey])
	}
	if copied[indexed.MetadataSourceRefsKey] != "nested reserved key is allowed" {
		t.Fatalf("nested reserved key metadata = %v", copied[indexed.MetadataSourceRefsKey])
	}

	obs.Metadata["nested"].(map[string]any)["k"] = "mutated-original"
	obs.Metadata["list"].([]any)[0].(map[string]any)["item"] = "mutated-original"
	if copied["nested"].(map[string]any)["k"] != "v" {
		t.Fatalf("copied metadata changed after original mutation: %#v", copied["nested"])
	}
	if copied["list"].([]any)[0].(map[string]any)["item"] != "v" {
		t.Fatalf("copied list metadata changed after original mutation: %#v", copied["list"])
	}

	copied["nested"].(map[string]any)["k"] = "mutated-copy"
	copied["list"].([]any)[0].(map[string]any)["item"] = "mutated-copy"
	if obs.Metadata["nested"].(map[string]any)["k"] != "mutated-original" {
		t.Fatalf("original metadata changed after copy mutation: %#v", obs.Metadata["nested"])
	}
	if obs.Metadata["list"].([]any)[0].(map[string]any)["item"] != "mutated-original" {
		t.Fatalf("original list metadata changed after copy mutation: %#v", obs.Metadata["list"])
	}
}

func TestProjectorLineageIsCloned(t *testing.T) {
	node := validSummaryNode()
	record, err := SummaryNode(node)
	if err != nil {
		t.Fatalf("SummaryNode() error = %v", err)
	}

	node.SourceRefs[0].Message.MessageID = "mutated-original"
	node.Signature.SourceRevisions[0].Revision = "mutated-original"
	node.Signature.DiagnosticSignatures["prompt"] = "mutated-original"
	if record.SourceRefs[0].Message.MessageID != "msg-1" {
		t.Fatalf("record source ref changed after original mutation: %+v", record.SourceRefs[0])
	}
	if record.Signature.SourceRevisions[0].Revision != "1" {
		t.Fatalf("record signature changed after original mutation: %+v", record.Signature)
	}
	if record.Signature.DiagnosticSignatures["prompt"] != "p1" {
		t.Fatalf("record diagnostics changed after original mutation: %+v", record.Signature)
	}

	record.SourceRefs[0].Message.MessageID = "mutated-record"
	record.Signature.SourceRevisions[0].Revision = "mutated-record"
	record.Signature.DiagnosticSignatures["prompt"] = "mutated-record"
	if node.SourceRefs[0].Message.MessageID != "mutated-original" {
		t.Fatalf("original source ref changed after record mutation: %+v", node.SourceRefs[0])
	}
	if node.Signature.SourceRevisions[0].Revision != "mutated-original" {
		t.Fatalf("original signature changed after record mutation: %+v", node.Signature)
	}
	if node.Signature.DiagnosticSignatures["prompt"] != "mutated-original" {
		t.Fatalf("original diagnostics changed after record mutation: %+v", node.Signature)
	}
}

func TestProjectorInvalidInputs(t *testing.T) {
	tests := []struct {
		name    string
		project func() (indexed.Record, error)
	}{
		{
			name: "empty chunk text",
			project: func() (indexed.Record, error) {
				chunk := validChunk()
				chunk.Text = ""
				return DocumentChunk(chunk)
			},
		},
		{
			name: "missing summary",
			project: func() (indexed.Record, error) {
				node := validSummaryNode()
				node.Summary = ""
				return SummaryNode(node)
			},
		},
		{
			name: "observation missing predicate",
			project: func() (indexed.Record, error) {
				obs := validObservation()
				obs.Predicate = ""
				return Observation(obs)
			},
		},
		{
			name: "observation missing signature",
			project: func() (indexed.Record, error) {
				obs := validObservation()
				obs.Signature = views.ViewSignature{}
				return Observation(obs)
			},
		},
		{
			name: "fact missing upstream refs",
			project: func() (indexed.Record, error) {
				record := validFact()
				record.Signature.UpstreamViewRefs = nil
				return FactRecord(record)
			},
		},
		{
			name: "fact node missing fact refs",
			project: func() (indexed.Record, error) {
				node := validFactNode()
				node.FactRefs = nil
				return FactNode(node)
			},
		},
		{
			name: "fact edge missing signature",
			project: func() (indexed.Record, error) {
				edge := validFactEdge()
				edge.Signature = views.ViewSignature{}
				return FactEdge(edge)
			},
		},
		{
			name: "profile missing fact refs",
			project: func() (indexed.Record, error) {
				profile := validProfile()
				profile.FactRefs = nil
				return EntityProfile(profile)
			},
		},
		{
			name: "profile missing signature",
			project: func() (indexed.Record, error) {
				profile := validProfile()
				profile.Signature = views.ViewSignature{}
				return EntityProfile(profile)
			},
		},
		{
			name: "event missing fact refs",
			project: func() (indexed.Record, error) {
				event := validEvent()
				event.FactRefs = nil
				return EntityEvent(event)
			},
		},
		{
			name: "event missing signature",
			project: func() (indexed.Record, error) {
				event := validEvent()
				event.Signature = views.ViewSignature{}
				return EntityEvent(event)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if record, err := tt.project(); err == nil {
				t.Fatalf("project() error = nil, want error; record = %+v", record)
			}
		})
	}
}

func validChunk() document.Chunk {
	ref := documentRef()
	return document.Chunk{
		ID:         "chunk-1",
		Scope:      validScope(),
		DocumentID: "doc-1",
		Layer: document.Layer{
			Name:    "default",
			Version: "v1",
		},
		Ordinal:   0,
		Span:      views.Span{Start: 0, End: 10},
		Text:      "chunk text",
		SourceRef: ref,
		Signature: views.ViewSignature{
			ViewID: "document-chunks",
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceDocument,
				SourceKey: ref.StableKey(),
				Revision:  "1",
			}},
			TransformSignature: "chunks:v1",
		},
		Metadata: sampleMetadata(),
	}
}

func validSummaryNode() recent.SummaryNode {
	ref := messageRef()
	return recent.SummaryNode{
		ID:         "summary-1",
		Scope:      validScope(),
		SourceRefs: []views.SourceRef{ref},
		Summary:    "summary text",
		Level:      1,
		Signature: views.ViewSignature{
			ViewID: "summary-dag",
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: ref.StableKey(),
				Revision:  "1",
			}},
			TransformSignature: "summary:v1",
			DiagnosticSignatures: map[string]string{
				"prompt": "p1",
			},
		},
		Metadata: sampleMetadata(),
	}
}

func validObservation() observation.Observation {
	ref := messageRef()
	return observation.Observation{
		ID:         "obs-1",
		Scope:      validScope(),
		Subject:    "user:1",
		Predicate:  "likes",
		Object:     "tea",
		Confidence: 0.7,
		SourceRefs: []views.SourceRef{ref},
		Signature: views.ViewSignature{
			ViewID: "observation-ledger",
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: ref.StableKey(),
				Revision:  "1",
			}},
			TransformSignature: "observation:v1",
		},
		Metadata: sampleMetadata(),
	}
}

func validFact() fact.Fact {
	from := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	return fact.Fact{
		ID:         "fact-1",
		Scope:      validScope(),
		Subject:    "user:1",
		Predicate:  "likes",
		Object:     "tea",
		Status:     fact.FactActive,
		Confidence: 0.8,
		ValidFrom:  &from,
		ObservationRefs: []fact.ObservationRef{{
			ObservationID: "obs-1",
			ScopeKind:     "conversation",
			ScopeID:       "conv-1",
		}},
		SourceRefs: []views.SourceRef{messageRef()},
		Signature:  upstreamSignature("fact-ledger", "observation-ledger"),
		Metadata:   sampleMetadata(),
	}
}

func validFactNode() fact.Node {
	return fact.Node{
		ID:      "entity:user-1",
		Scope:   validScope(),
		Kind:    fact.NodeEntity,
		Label:   "User One",
		Aliases: []string{"U1", "User 1"},
		FactRefs: []fact.FactRef{{
			FactID: "fact-1",
			Role:   "subject",
		}},
		SourceRefs: []views.SourceRef{messageRef()},
		Signature:  upstreamSignature("fact-graph", "fact-ledger"),
		Metadata:   sampleMetadata(),
	}
}

func validFactEdge() fact.Edge {
	until := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	return fact.Edge{
		ID:         "edge-1",
		Scope:      validScope(),
		From:       "entity:user-1",
		To:         "value:tea",
		Predicate:  "likes",
		Status:     fact.FactActive,
		Confidence: 0.9,
		ValidUntil: &until,
		FactRefs: []fact.FactRef{{
			FactID: "fact-1",
			Role:   "supporting",
		}},
		SourceRefs: []views.SourceRef{messageRef()},
		Signature:  upstreamSignature("fact-graph", "fact-ledger"),
		Metadata:   sampleMetadata(),
	}
}

func validScope() views.Scope {
	return views.Scope{
		RuntimeID:      "runtime-1",
		UserID:         "user-1",
		AgentID:        "agent-1",
		ConversationID: "conv-1",
		DatasetID:      "dataset-1",
		EntityID:       "entity:user-1",
	}
}

func assertMetadata(t *testing.T, record indexed.Record, key string, want any) {
	t.Helper()
	if got := record.Metadata[key]; got != want {
		t.Fatalf("metadata %s = %v, want %v", key, got, want)
	}
}

func validProfile() entity.ProfileRecord {
	return entity.ProfileRecord{
		ID:      "profile-1",
		Scope:   validScope(),
		Label:   "User One",
		Summary: "likes tea",
		Slots: []entity.Slot{{
			Name:       "favorite_drink",
			Value:      "tea",
			Confidence: 0.8,
			FactRefs:   []fact.FactRef{{FactID: "fact-1", Role: "supporting"}},
		}},
		FactRefs:   []fact.FactRef{{FactID: "fact-1", Role: "supporting"}},
		SourceRefs: []views.SourceRef{messageRef()},
		Signature:  upstreamSignature("entity-profile", "fact-graph"),
		Metadata:   sampleMetadata(),
	}
}

func validEvent() entity.Event {
	occurred := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	return entity.Event{
		ID:          "event-1",
		Scope:       validScope(),
		Title:       "Tried tea",
		Description: "user tried tea",
		OccurredAt:  &occurred,
		FactRefs:    []fact.FactRef{{FactID: "fact-1", Role: "supporting"}},
		SourceRefs:  []views.SourceRef{messageRef()},
		Signature:   upstreamSignature("entity-timeline", "fact-graph"),
		Metadata:    sampleMetadata(),
	}
}

func messageRef() views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conv-1",
			MessageID:      "msg-1",
			Span:           &views.Span{Start: 1, End: 3},
		},
	}
}

func documentRef() views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:  "dataset-1",
			DocumentID: "doc-1",
			Span:       &views.Span{Start: 0, End: 10},
		},
	}
}

func setChunkIdentity(chunk *document.Chunk, datasetID, documentID string, id document.ChunkID) {
	chunk.Scope.DatasetID = datasetID
	chunk.DocumentID = documentID
	chunk.ID = id
	chunk.SourceRef.Document.DatasetID = datasetID
	chunk.SourceRef.Document.DocumentID = documentID
	chunk.Signature.SourceRevisions[0].SourceKey = chunk.SourceRef.StableKey()
}

func setSummaryNodeIdentity(node *recent.SummaryNode, conversationID string, id recent.NodeID) {
	node.Scope.ConversationID = conversationID
	node.ID = id
	node.SourceRefs[0].Message.ConversationID = conversationID
	node.Signature.SourceRevisions[0].SourceKey = node.SourceRefs[0].StableKey()
}

func upstreamSignature(viewID views.ID, upstreamID views.ID) views.ViewSignature {
	return views.ViewSignature{
		ViewID: viewID,
		UpstreamViewRefs: []views.UpstreamViewRef{{
			ViewID:          upstreamID,
			OutputSignature: string(upstreamID) + ":v1",
			RecordKey:       "record-1",
		}},
		TransformSignature: string(viewID) + ":v1",
		DiagnosticSignatures: map[string]string{
			"projector": "test",
		},
	}
}

func sampleMetadata() map[string]any {
	return map[string]any{
		"k": "v",
		"nested": map[string]any{
			"k": "v",
		},
		"list": []any{"v"},
	}
}

func TestHelpersDoNotMutateExpectedFixtures(t *testing.T) {
	original := sampleMetadata()
	cloned := cloneAnyMap(original)
	cloned["nested"].(map[string]any)["k"] = "changed"
	if reflect.DeepEqual(original, cloned) {
		t.Fatal("cloneAnyMap returned shared nested metadata")
	}
}
