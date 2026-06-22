package projectors

import (
	"strings"
	"testing"
	"time"

	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSupportedRecordTypes(t *testing.T) {
	if RecordTypeDocumentChunk != "document_chunk" {
		t.Fatalf("RecordTypeDocumentChunk = %q", RecordTypeDocumentChunk)
	}
	if RecordTypeSummaryNode != "summary_node" {
		t.Fatalf("RecordTypeSummaryNode = %q", RecordTypeSummaryNode)
	}
	if RecordTypeSourceMessage != "source_message" {
		t.Fatalf("RecordTypeSourceMessage = %q", RecordTypeSourceMessage)
	}
}

func TestDocumentChunkRecordIDMatchesDocumentChunkProjection(t *testing.T) {
	span := views.Span{Start: 0, End: 11}
	sourceRef := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:   "dataset",
			DocumentID:  "doc-1",
			Version:     "1",
			ContentHash: "sha256:doc",
			Span:        &span,
		},
	}
	chunk := viewdocument.Chunk{
		ID:         "chunk-1",
		Scope:      views.Scope{RuntimeID: "rt", UserID: "user", DatasetID: "dataset"},
		DocumentID: "doc-1",
		Layer: viewdocument.Layer{
			Name:               "paragraph",
			Version:            "v1",
			TransformSignature: "paragraph:v1",
		},
		Ordinal:   0,
		Span:      span,
		Text:      "hello world",
		SourceRef: sourceRef,
		Signature: views.ViewSignature{
			ViewID:             viewdocument.DefaultChunksID,
			TransformSignature: "paragraph:v1",
			SourceRevisions: []views.SourceRevision{{
				Kind:        views.SourceDocument,
				SourceKey:   sourceRef.StableKey(),
				Revision:    "1",
				ContentHash: "sha256:doc",
			}},
		},
	}

	record, err := DocumentChunk(chunk)
	if err != nil {
		t.Fatalf("DocumentChunk() error = %v", err)
	}
	if got, want := record.ID, DocumentChunkRecordID("dataset", "doc-1", "chunk-1"); got != want {
		t.Fatalf("DocumentChunk record ID = %q, want helper %q", got, want)
	}

	invalid := chunk
	invalid.Text = ""
	if got := DocumentChunkRecordID(invalid.Scope.DatasetID, invalid.DocumentID, invalid.ID); got != record.ID {
		t.Fatalf("DocumentChunkRecordID invalid chunk fields = %q, want %q", got, record.ID)
	}
	if _, err := DocumentChunk(invalid); err == nil {
		t.Fatal("DocumentChunk invalid chunk error = nil, want validation")
	}
}

func TestSourceMessageRecordsProjectsSearchableTextAndMetadata(t *testing.T) {
	createdAt := time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC)
	msg := sourcemessage.Message{
		ID:             "dia-1",
		ConversationID: "conv-1",
		Seq:            7,
		Message: model.Message{
			Role: model.RoleUser,
			Parts: []model.Part{
				{Type: model.PartData, Data: &model.DataRef{
					MimeType: "application/flowcraft.history-message+json",
					Value: map[string]any{
						"dia_id":           "dia-1",
						"speaker_name":     "Ada",
						"session_datetime": "2024-01-02 03:04",
						"image_caption":    "red mug",
						"image_query":      "what is on the table?",
					},
				}},
				{Type: model.PartText, Text: "Ada saw a red mug."},
			},
		},
		Metadata: map[string]any{
			"dia_id":       "dia-1",
			"speaker":      "Ada",
			"session":      1,
			"blip_caption": "red mug",
			"query":        "what is on the table?",
			"nested":       map[string]any{"k": "v"},
		},
		CreatedAt: createdAt,
	}
	records, err := SourceMessageRecords(views.Scope{
		RuntimeID:      "rt",
		UserID:         "user",
		AgentID:        "agent",
		ConversationID: "conv-1",
		DatasetID:      "dataset",
	}, msg)
	if err != nil {
		t.Fatalf("SourceMessageRecords() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("SourceMessageRecords() len = %d, want 1", len(records))
	}
	record := records[0]
	for _, want := range []string{"role: user", "Ada saw a red mug.", "session_datetime", "red mug", "what is on the table?"} {
		if !strings.Contains(record.Text, want) {
			t.Fatalf("record text = %q, want %q", record.Text, want)
		}
	}
	if record.Metadata[MetadataRecordTypeKey] != RecordTypeSourceMessage {
		t.Fatalf("record type metadata = %v", record.Metadata[MetadataRecordTypeKey])
	}
	for key, want := range map[string]any{
		MetadataRuntimeIDKey:      "rt",
		MetadataUserIDKey:         "user",
		MetadataAgentIDKey:        "agent",
		MetadataConversationIDKey: "conv-1",
		MetadataDatasetIDKey:      "dataset",
		MetadataMessageIDKey:      "dia-1",
		MetadataMessageSeqKey:     uint64(7),
		MetadataCreatedAtKey:      createdAt.Format(time.RFC3339Nano),
		"dia_id":                  "dia-1",
		"speaker":                 "Ada",
		"session_datetime":        "2024-01-02 03:04",
		"caption":                 "red mug",
		"message_id":              "dia-1",
		MetadataMessageChunkIndex: 0,
		MetadataMessageChunkCount: 1,
		MetadataMessageChunkStart: 0,
	} {
		if got := record.Metadata[key]; got != want {
			t.Fatalf("metadata[%q] = %#v, want %#v", key, got, want)
		}
	}
	if got := record.Metadata[MetadataMessageChunkEnd]; got == nil {
		t.Fatalf("metadata[%q] is nil, want chunk end", MetadataMessageChunkEnd)
	}
	if got, want := record.ID, SourceMessageChunkRecordID("dataset", "agent", "conv-1", "dia-1", 0); got != want {
		t.Fatalf("record ID = %q, want %q", got, want)
	}
	record.Metadata[MetadataRecordMetadataKey].(map[string]any)["nested"].(map[string]any)["k"] = "mutated"
	if msg.Metadata["nested"].(map[string]any)["k"] != "v" {
		t.Fatalf("source metadata was mutated: %+v", msg.Metadata)
	}
}

func TestSourceMessageRecordsChunkLongMessage(t *testing.T) {
	msg := sourcemessage.Message{
		ID:             "dia-long",
		ConversationID: "conv",
		Message:        model.NewTextMessage(model.RoleUser, strings.Repeat("alpha beta gamma. ", 900)),
	}
	scope := views.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent", ConversationID: "conv", DatasetID: "dataset"}

	records, err := SourceMessageRecords(scope, msg)
	if err != nil {
		t.Fatalf("SourceMessageRecords(long) error = %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("SourceMessageRecords(long) len = %d, want multiple chunks", len(records))
	}
	for i, record := range records {
		if got, want := record.ID, SourceMessageChunkRecordID("dataset", "agent", "conv", "dia-long", i); got != want {
			t.Fatalf("record[%d].ID = %q, want %q", i, got, want)
		}
		if got, want := record.Metadata[MetadataMessageChunkIndex], i; got != want {
			t.Fatalf("record[%d] chunk index = %v, want %d", i, got, i)
		}
		if got, want := record.Metadata[MetadataMessageChunkCount], len(records); got != want {
			t.Fatalf("record[%d] chunk count = %v, want %d", i, got, len(records))
		}
		if len(record.Text) == 0 {
			t.Fatalf("record[%d] text is empty", i)
		}
		if len(record.SourceRefs) != 1 || record.SourceRefs[0].Message == nil || record.SourceRefs[0].Message.MessageID != "dia-long" {
			t.Fatalf("record[%d] source refs = %+v, want original message ref", i, record.SourceRefs)
		}
	}
}

func TestSourceMessageRecordsRecordIDIncludesAgentScope(t *testing.T) {
	msg := sourcemessage.Message{
		ID:             "dia-1",
		ConversationID: "conv",
		Message:        model.NewTextMessage(model.RoleUser, "same message id across agents"),
	}
	scopeA := views.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent-a", ConversationID: "conv", DatasetID: "dataset"}
	scopeB := scopeA
	scopeB.AgentID = "agent-b"

	recordsA, err := SourceMessageRecords(scopeA, msg)
	if err != nil {
		t.Fatalf("SourceMessageRecords(agent-a) error = %v", err)
	}
	recordsB, err := SourceMessageRecords(scopeB, msg)
	if err != nil {
		t.Fatalf("SourceMessageRecords(agent-b) error = %v", err)
	}
	recordA := recordsA[0]
	recordB := recordsB[0]
	if recordA.ID == recordB.ID {
		t.Fatalf("SourceMessageRecords record IDs are equal across agents: %q", recordA.ID)
	}
	if got, want := recordA.Metadata[MetadataAgentIDKey], "agent-a"; got != want {
		t.Fatalf("agent-a metadata[%q] = %v, want %q", MetadataAgentIDKey, got, want)
	}
	if got, want := recordB.Metadata[MetadataAgentIDKey], "agent-b"; got != want {
		t.Fatalf("agent-b metadata[%q] = %v, want %q", MetadataAgentIDKey, got, want)
	}
	if got, want := recordB.Metadata[MetadataMessageIDKey], "dia-1"; got != want {
		t.Fatalf("agent-b metadata[%q] = %v, want %q", MetadataMessageIDKey, got, want)
	}
}

func TestSummaryNodeRecordIDIncludesAgentScope(t *testing.T) {
	scopeA := views.Scope{RuntimeID: "rt", UserID: "user", AgentID: "agent-a", ConversationID: "conv"}
	scopeB := scopeA
	scopeB.AgentID = "agent-b"
	nodeA := summaryNodeForProjectorTest(scopeA, "summary-1", "agent a summary")
	nodeB := summaryNodeForProjectorTest(scopeB, "summary-1", "agent b summary")

	recordA, err := SummaryNode(nodeA)
	if err != nil {
		t.Fatalf("SummaryNode(agent-a) error = %v", err)
	}
	recordB, err := SummaryNode(nodeB)
	if err != nil {
		t.Fatalf("SummaryNode(agent-b) error = %v", err)
	}
	if recordA.ID == recordB.ID {
		t.Fatalf("SummaryNode record IDs are equal across agents: %q", recordA.ID)
	}
	if got, want := recordA.Metadata[MetadataAgentIDKey], "agent-a"; got != want {
		t.Fatalf("agent-a metadata[%q] = %v, want %q", MetadataAgentIDKey, got, want)
	}
	if got, want := recordB.Metadata[MetadataAgentIDKey], "agent-b"; got != want {
		t.Fatalf("agent-b metadata[%q] = %v, want %q", MetadataAgentIDKey, got, want)
	}
	if got, want := recordB.Metadata[MetadataNodeIDKey], "summary-1"; got != want {
		t.Fatalf("agent-b metadata[%q] = %v, want %q", MetadataNodeIDKey, got, want)
	}
}

func summaryNodeForProjectorTest(scope views.Scope, id recent.NodeID, summary string) recent.SummaryNode {
	ref := views.SourceRef{
		Kind:    views.SourceMessage,
		Message: &views.MessageSourceRef{ConversationID: scope.ConversationID, MessageID: "dia-1"},
	}
	return recent.SummaryNode{
		ID:         id,
		Scope:      scope,
		SourceRefs: []views.SourceRef{ref},
		Summary:    summary,
		Signature: views.ViewSignature{
			ViewID: "summary_dag",
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: ref.StableKey(),
				Revision:  "1",
			}},
			TransformSignature: "test-summary:v1",
		},
	}
}
