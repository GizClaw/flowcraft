package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

type capabilities struct{}

func (capabilities) Context(context.Context, memory.ContextRequest) (memory.ContextResult, error) {
	return memory.ContextResult{}, nil
}

func (capabilities) CommitTurn(context.Context, memory.Turn) error { return nil }

func (capabilities) PutDocument(context.Context, memory.Document) error { return nil }

var (
	_ memory.ContextProvider = capabilities{}
	_ memory.TurnSink        = capabilities{}
	_ memory.DocumentSink    = capabilities{}
)

func TestCapabilityRequestsValidate(t *testing.T) {
	scope := memory.Scope{RuntimeID: "runtime", UserID: "user"}
	content := message.NewTextMessage(message.RoleUser, "hello").Content

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "context",
			err: (memory.ContextRequest{
				Scope:    scope,
				Query:    "what matters?",
				MinScore: 0.5,
			}).Validate(),
		},
		{
			name: "turn",
			err: (memory.Turn{
				Scope:          scope,
				ConversationID: "conversation",
				IdempotencyKey: "run",
				Messages:       []message.Message{message.NewTextMessage(message.RoleUser, "hello")},
			}).Validate(),
		},
		{
			name: "document",
			err: (memory.Document{
				Scope:          scope,
				DatasetID:      "dataset",
				DocumentID:     "document",
				IdempotencyKey: "ingest",
				Content:        content,
				Provenance: []memory.SourceRef{{
					Kind: memory.SourceExternal,
					ID:   "source",
				}},
			}).Validate(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.err != nil {
				t.Fatalf("Validate() error = %v", test.err)
			}
		})
	}
}

func TestHardPartitionKeyIncludesAgentID(t *testing.T) {
	global := memory.Scope{RuntimeID: "runtime", UserID: "user"}
	agentA := memory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent-a"}
	agentB := memory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent-b"}

	if global.HardPartitionKey() == agentA.HardPartitionKey() ||
		agentA.HardPartitionKey() == agentB.HardPartitionKey() {
		t.Fatal("HardPartitionKey aliases distinct agent partitions")
	}
}

func TestCapabilityRequestsRejectMissingAddresses(t *testing.T) {
	scope := memory.Scope{RuntimeID: "runtime"}
	content := message.NewTextMessage(message.RoleUser, "hello").Content

	tests := []struct {
		name string
		err  error
	}{
		{"context query", (memory.ContextRequest{Scope: scope}).Validate()},
		{"turn conversation", (memory.Turn{
			Scope:          scope,
			IdempotencyKey: "run",
			Messages:       []message.Message{message.NewTextMessage(message.RoleUser, "hello")},
		}).Validate()},
		{"document provenance", (memory.Document{
			Scope:          scope,
			DatasetID:      "dataset",
			DocumentID:     "document",
			IdempotencyKey: "ingest",
			Content:        content,
		}).Validate()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !memory.IsKind(test.err, memory.KindInvalidRequest) {
				t.Fatalf("Validate() error = %v, want invalid request", test.err)
			}
			if !errdefs.IsValidation(test.err) {
				t.Fatalf("Validate() error = %v, want validation classification", test.err)
			}
		})
	}
}

func TestTurnCloneOwnsMutableData(t *testing.T) {
	original := memory.Turn{
		Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hello")},
		Metadata: memory.Metadata{"key": "value"},
	}
	cloned := original.Clone()

	cloned.Metadata["key"] = "changed"
	cloned.Messages[0] = message.NewTextMessage(message.RoleAssistant, "changed")

	if original.Metadata["key"] != "value" {
		t.Fatal("Clone() aliases metadata")
	}
	if original.Messages[0].Role != message.RoleUser {
		t.Fatal("Clone() aliases messages")
	}
}

func TestContextItemRequiresNormalizedScoreAndProvenance(t *testing.T) {
	valid := memory.ContextItem{
		ID:          "fact-1",
		Kind:        memory.ContextFact,
		Content:     message.NewTextMessage(message.RoleAssistant, "A fact").Content,
		Score:       0.75,
		Sources:     []memory.SourceRef{{Kind: memory.SourceMessage, ID: "message-1"}},
		SourceClass: memory.ContextSourceLongTerm,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	invalid := valid
	invalid.Score = 2
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted an out-of-range score")
	}

	invalid = valid
	invalid.Sources = nil
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted missing provenance")
	}
}

func TestContextAddressQualifiesLocalIdentity(t *testing.T) {
	scope := memory.Scope{RuntimeID: "runtime", UserID: "user"}
	first := memory.ContextAddress{Kind: memory.ContextFact, ConversationID: "a", ItemID: "fact"}
	second := memory.ContextAddress{Kind: memory.ContextFact, ConversationID: "b", ItemID: "fact"}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.Key() == second.Key() || first.Identity(scope) == second.Identity(scope) {
		t.Fatal("conversation-qualified addresses collided")
	}
	otherDataset := memory.ContextAddress{
		Kind: memory.ContextDocumentChunk, DatasetID: "other", DocumentID: "doc", ItemID: "chunk",
	}
	document := memory.ContextAddress{
		Kind: memory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "doc", ItemID: "chunk",
	}
	if document.Identity(scope) == otherDataset.Identity(scope) {
		t.Fatal("dataset-qualified addresses collided")
	}
}

func TestClosedMemoryKindsValidate(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantErr  bool
		validate func() error
	}{
		{"source message", string(memory.SourceMessage), false, memory.SourceMessage.Validate},
		{"source document", string(memory.SourceDocument), false, memory.SourceDocument.Validate},
		{"source external", string(memory.SourceExternal), false, memory.SourceExternal.Validate},
		{"source empty", "", true, memory.SourceKind("").Validate},
		{"source unknown", "unknown", true, memory.SourceKind("unknown").Validate},
		{"source untrimmed", " message ", true, memory.SourceKind(" message ").Validate},
		{"item raw message", string(memory.ContextRawMessage), false, memory.ContextRawMessage.Validate},
		{"item fact", string(memory.ContextFact), false, memory.ContextFact.Validate},
		{"item document resource", string(memory.ContextDocumentResource), false, memory.ContextDocumentResource.Validate},
		{"item document section", string(memory.ContextDocumentSection), false, memory.ContextDocumentSection.Validate},
		{"item document chunk", string(memory.ContextDocumentChunk), false, memory.ContextDocumentChunk.Validate},
		{"item document summary", string(memory.ContextDocumentSummary), false, memory.ContextDocumentSummary.Validate},
		{"item summary", string(memory.ContextSummary), false, memory.ContextSummary.Validate},
		{"item empty", "", true, memory.ContextItemKind("").Validate},
		{"item unknown", "unknown", true, memory.ContextItemKind("unknown").Validate},
		{"item untrimmed", " fact ", true, memory.ContextItemKind(" fact ").Validate},
		{"source class recent", string(memory.ContextSourceRecent), false, memory.ContextSourceRecent.Validate},
		{"source class long term", string(memory.ContextSourceLongTerm), false, memory.ContextSourceLongTerm.Validate},
		{"source class summary", string(memory.ContextSourceSummary), false, memory.ContextSourceSummary.Validate},
		{"source class empty", "", true, memory.ContextSourceClass("").Validate},
		{"source class unknown", "unknown", true, memory.ContextSourceClass("unknown").Validate},
		{"source class untrimmed", " recent ", true, memory.ContextSourceClass(" recent ").Validate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("%s.Validate() error = %v, wantErr %v", test.value, err, test.wantErr)
			}
		})
	}
}

func TestCompositeValidationRejectsUnknownKinds(t *testing.T) {
	source := memory.SourceRef{Kind: memory.SourceKind("unknown"), ID: "source"}
	if err := source.Validate(); err == nil {
		t.Fatal("SourceRef.Validate() accepted an unknown source kind")
	}

	item := memory.ContextItem{
		ID:          "fact-1",
		Kind:        memory.ContextItemKind("unknown"),
		Content:     message.NewTextMessage(message.RoleAssistant, "A fact").Content,
		Sources:     []memory.SourceRef{{Kind: memory.SourceMessage, ID: "message-1"}},
		SourceClass: memory.ContextSourceLongTerm,
	}
	if err := item.Validate(); err == nil {
		t.Fatal("ContextItem.Validate() accepted an unknown item kind")
	}

	item.Kind = memory.ContextFact
	item.SourceClass = memory.ContextSourceClass("unknown")
	if err := item.Validate(); err == nil {
		t.Fatal("ContextItem.Validate() accepted an unknown source class")
	}
}

func TestErrorPreservesClassificationAndCause(t *testing.T) {
	cause := errors.New("backend unavailable")
	err := memory.NewError(memory.KindProviderFailure, "context", cause)

	if !memory.IsKind(err, memory.KindProviderFailure) {
		t.Fatalf("IsKind() = false")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("IsNotAvailable() = false")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is() = false")
	}
}
