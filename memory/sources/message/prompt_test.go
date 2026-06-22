package message

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestPromptMessageFromSourcePreservesOriginalPartsAndAppendsMetadata(t *testing.T) {
	createdAt := time.Date(2026, 6, 18, 9, 10, 11, 0, time.UTC)
	msg := Message{
		ID:             "msg-1",
		ConversationID: "conv-1",
		Seq:            42,
		Message: model.Message{
			Role: model.RoleUser,
			Parts: []model.Part{
				{Type: model.PartText, Text: "hello"},
				{Type: model.PartImage, Image: &model.MediaRef{URL: "https://example.test/image.png", MediaType: "image/png"}},
			},
		},
		Metadata: map[string]any{
			"speaker": "Ada",
			"nested":  map[string]any{"topic": "tea"},
			"tags":    []any{"memory", map[string]any{"kind": "preference"}},
		},
		SpanRefs:  []SpanRef{{MessageID: "msg-1", Start: 2, End: 7}},
		CreatedAt: createdAt,
	}

	got := PromptMessageFromSource(msg)

	if got.Role != model.RoleUser {
		t.Fatalf("Role = %q, want user", got.Role)
	}
	if len(got.Parts) != 3 {
		t.Fatalf("Parts len = %d, want original two parts plus metadata", len(got.Parts))
	}
	if got.Parts[0].Type != model.PartText || got.Parts[0].Text != "hello" {
		t.Fatalf("first part = %+v, want original text part", got.Parts[0])
	}
	if got.Parts[1].Type != model.PartImage || got.Parts[1].Image == nil || got.Parts[1].Image.URL != "https://example.test/image.png" {
		t.Fatalf("second part = %+v, want original image part", got.Parts[1])
	}
	dataPart := got.Parts[2]
	if dataPart.Type != model.PartData || dataPart.Data == nil {
		t.Fatalf("metadata part = %+v, want data part", dataPart)
	}
	if dataPart.Data.MimeType != PromptSourceMessageMIMEType {
		t.Fatalf("metadata MIME = %q, want %q", dataPart.Data.MimeType, PromptSourceMessageMIMEType)
	}
	value := dataPart.Data.Value
	if value["source_id"] != "msg-1" || value["conversation_id"] != "conv-1" || value["seq"] != uint64(42) {
		t.Fatalf("source metadata = %+v, want source id, conversation id, and seq", value)
	}
	if value["created_at"] != "2026-06-18T09:10:11Z" {
		t.Fatalf("created_at = %v, want RFC3339 string", value["created_at"])
	}
	metadata := value["metadata"].(map[string]any)
	if metadata["speaker"] != "Ada" || metadata["nested"].(map[string]any)["topic"] != "tea" {
		t.Fatalf("metadata = %+v, want cloned source metadata", metadata)
	}
	spanRefs := value["span_refs"].([]any)
	spanRef := spanRefs[0].(map[string]any)
	if spanRef["message_id"] != "msg-1" || spanRef["start"] != 2 || spanRef["end"] != 7 {
		t.Fatalf("span_refs = %+v, want cloned source spans", spanRefs)
	}
}

func TestPromptMessageFromSourceDoesNotShareMutableMetadata(t *testing.T) {
	msg := Message{
		ID:             "msg-1",
		ConversationID: "conv-1",
		Seq:            1,
		Message:        model.NewTextMessage(model.RoleUser, "hello"),
		Metadata: map[string]any{
			"nested": map[string]any{"k": "v"},
			"items":  []any{map[string]any{"name": "first"}},
		},
		SpanRefs: []SpanRef{{MessageID: "msg-1", Start: 0, End: 5}},
	}

	got := PromptMessageFromSource(msg)
	msg.Parts[0].Text = "mutated"
	msg.Metadata["nested"].(map[string]any)["k"] = "mutated"
	msg.Metadata["items"].([]any)[0].(map[string]any)["name"] = "mutated"
	msg.SpanRefs[0].Start = 99

	if got.Parts[0].Text != "hello" {
		t.Fatalf("original text part shared mutation: %q", got.Parts[0].Text)
	}
	value := got.Parts[len(got.Parts)-1].Data.Value
	metadata := value["metadata"].(map[string]any)
	if got := metadata["nested"].(map[string]any)["k"]; got != "v" {
		t.Fatalf("nested metadata shared mutation: %v", got)
	}
	if got := metadata["items"].([]any)[0].(map[string]any)["name"]; got != "first" {
		t.Fatalf("slice metadata shared mutation: %v", got)
	}
	spanRef := value["span_refs"].([]any)[0].(map[string]any)
	if got := spanRef["start"]; got != 0 {
		t.Fatalf("span refs shared mutation: %v", got)
	}
}
