package message_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestContentRoundTripsEveryCanonicalPart(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	audio, err := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	video, err := media.NewVideoURL("https://example.com/video.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	call, err := message.NewCall("call-1", "search", map[string]any{"query": "cat"})
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	content := message.Content{Parts: []message.Part{message.TextPart{Text: "hello"},
		message.ImagePart{Source: image},
		message.AudioPart{Source: audio},
		message.VideoPart{Source: video},
		message.FilePart{URI: "s3://bucket/document.pdf", MediaType: "application/pdf", Name: "document.pdf"},
		message.DataPart{MediaType: "application/json", Value: json.RawMessage(`{"answer":42}`)},
		message.ToolCallPart{Call: call},
		message.ToolResultPart{Result: message.Result{CallID: "call-1", Content: "found"}},
		message.ReasoningPart{Text: "let me think", Signature: "sig-1"}},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded message.Content
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantKinds := []message.PartKind{
		message.PartText, message.PartImage, message.PartAudio, message.PartVideo,
		message.PartFile, message.PartData, message.PartToolCall, message.PartToolResult,
		message.PartReasoning,
	}
	if len(decoded.Parts) != len(wantKinds) {
		t.Fatalf("decoded part count = %d, want %d", len(decoded.Parts), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got := decoded.Parts[i].Kind(); got != want {
			t.Fatalf("decoded part %d kind = %q, want %q", i, got, want)
		}
	}

	clone := decoded.Clone()
	clone.Parts[5].(message.DataPart).Value[0] = '['
	clone.Parts[6].(message.ToolCallPart).Call.Arguments[0] = '['
	if string(decoded.Parts[5].(message.DataPart).Value) != `{"answer":42}` {
		t.Fatal("data part clone mutated source")
	}
	if string(decoded.Parts[6].(message.ToolCallPart).Call.Arguments) != `{"query":"cat"}` {
		t.Fatal("tool call part clone mutated source")
	}
}

func TestContentAcceptsPointerPartsConsistently(t *testing.T) {
	plain := message.Content{Parts: []message.Part{
		message.TextPart{Text: "hello"},
	}}
	if err := plain.Validate(); err != nil {
		t.Fatalf("Validate plain: %v", err)
	}

	pointer := message.Content{Parts: []message.Part{
		&message.TextPart{Text: "hello"},
	}}
	if err := pointer.Validate(); err != nil {
		t.Fatalf("Validate pointer: %v", err)
	}
	if plain.Text() != pointer.Text() {
		t.Fatalf("pointer/value Text() mismatch: %q vs %q", plain.Text(), pointer.Text())
	}
}

func TestContentRejectsEmptyAndUnknown(t *testing.T) {
	if err := (message.Content{}).Validate(); err == nil {
		t.Fatal("empty content must be invalid")
	}
	raw := json.RawMessage(`[{"type":"bogus","text":"x"}]`)
	var decoded message.Content
	if err := json.Unmarshal(raw, &decoded); err == nil {
		t.Fatal("unknown part kind must be rejected on unmarshal")
	}
}

func TestMessageValidationRoles(t *testing.T) {
	userText := message.NewTextMessage(message.RoleUser, "hi")
	if err := userText.Validate(); err != nil {
		t.Fatalf("text message: %v", err)
	}
	if userText.ToolCalls() != nil {
		t.Fatal("text message must report no tool calls")
	}
	if userText.HasToolCalls() {
		t.Fatal("text message must not have tool calls")
	}

	call, _ := message.NewCall("c1", "search", map[string]any{"q": "x"})
	assistant := message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: call},
		}},
	}
	if err := assistant.Validate(); err != nil {
		t.Fatalf("assistant tool calls: %v", err)
	}
	if calls := assistant.ToolCalls(); len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("ToolCalls = %+v", calls)
	}

	toolResult := message.Message{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.Result{CallID: "c1", Content: "ok"}},
		}},
	}
	if err := toolResult.Validate(); err != nil {
		t.Fatalf("tool result message: %v", err)
	}
	if results := toolResult.ToolResults(); len(results) != 1 || results[0].CallID != "c1" {
		t.Fatalf("ToolResults = %+v", results)
	}

	if err := (message.Message{
		Role:    message.RoleTool,
		Content: message.Content{Parts: []message.Part{message.TextPart{Text: "x"}}},
	}).Validate(); err == nil {
		t.Fatal("tool role with text only must be invalid")
	}
}

func TestPointerToolCallPartRoundTrip(t *testing.T) {
	call, _ := message.NewCall("c1", "search", map[string]any{"q": "x"})
	msg := message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			&message.ToolCallPart{Call: call},
		}},
	}
	if err := msg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !msg.HasToolCalls() {
		t.Fatal("HasToolCalls must see pointer tool call part")
	}
}

func TestReasoningPartValidationAndRoles(t *testing.T) {
	if err := (message.ReasoningPart{}).Validate(); err == nil {
		t.Fatal("empty reasoning part must be invalid")
	}
	if err := (message.ReasoningPart{Signature: "sig"}).Validate(); err != nil {
		t.Fatalf("redacted reasoning (signature only) must be valid: %v", err)
	}
	if err := (message.ReasoningPart{Text: "thinking"}).Validate(); err != nil {
		t.Fatalf("unsigned reasoning must be valid: %v", err)
	}

	reasoning := message.Content{Parts: []message.Part{message.ReasoningPart{Text: "t", Signature: "s"}}}
	if err := (message.Message{Role: message.RoleAssistant, Content: reasoning}).Validate(); err != nil {
		t.Fatalf("assistant message must accept reasoning: %v", err)
	}
	for _, role := range []message.Role{message.RoleSystem, message.RoleUser, message.RoleTool} {
		if err := (message.Message{Role: role, Content: reasoning}).Validate(); err == nil {
			t.Fatalf("%s message must reject reasoning parts", role)
		}
	}
}

func TestReasoningPartRoundTripsSignature(t *testing.T) {
	content := message.Content{Parts: []message.Part{
		message.ReasoningPart{Text: "visible", Signature: "sig-1"},
		message.ReasoningPart{Signature: "opaque-redacted-data"},
	}}
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded message.Content
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	first := decoded.Parts[0].(message.ReasoningPart)
	if first.Text != "visible" || first.Signature != "sig-1" {
		t.Fatalf("signed reasoning round-trip = %#v", first)
	}
	second := decoded.Parts[1].(message.ReasoningPart)
	if second.Text != "" || second.Signature != "opaque-redacted-data" {
		t.Fatalf("redacted reasoning round-trip = %#v", second)
	}
}

func TestDefinitionAndCallValidation(t *testing.T) {
	if err := (message.Definition{Name: ""}).Validate(); err == nil {
		t.Fatal("empty name must be invalid")
	}
	if err := (message.Definition{Name: "x", InputSchema: json.RawMessage(`"not object"`)}).Validate(); err == nil {
		t.Fatal("non-object schema must be invalid")
	}
	if err := (message.Definition{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}).Validate(); err != nil {
		t.Fatalf("valid definition: %v", err)
	}

	if err := (message.Call{}).Validate(); err == nil {
		t.Fatal("empty call must be invalid")
	}
	call, err := message.NewCall("c1", "search", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	cloned := call.Clone()
	cloned.Arguments[0] = '['
	if string(call.Arguments) == string(cloned.Arguments) {
		t.Fatal("Clone did not deep-copy arguments")
	}
	if err := (message.Result{}).Validate(); err == nil {
		t.Fatal("empty result must be invalid")
	}
}

func TestSchemaBuilderProducesDefinition(t *testing.T) {
	def := message.DefineSchema("search", "search the web",
		message.ToolProperty("query", "string", "search query"),
		message.ToolPropertyWithDefault("limit", "integer", "result limit", 10),
		message.ToolArrayProperty("filters", "filters to apply", message.Items("string")),
		message.ToolObjectProperty("meta", "metadata", map[string]any{
			"trace_id": map[string]any{"type": "string"},
		}),
		message.ToolEnumProperty("mode", "string", "search mode", "fast", "deep"),
		message.ToolStringMapProperty("headers", "extra headers"),
	).Required("query", "mode").DisallowAdditionalProperties().Build()
	if def.Name != "search" {
		t.Fatalf("name = %q", def.Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("schema not JSON: %v", err)
	}
	for _, key := range []string{"query", "limit", "filters", "meta", "mode", "headers"} {
		if _, ok := schema["properties"].(map[string]any)[key]; !ok {
			t.Fatalf("schema missing %q: %v", key, schema)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("DisallowAdditionalProperties not applied: %v", schema)
	}
}

func TestMessageHelpers(t *testing.T) {
	msgs := []message.Message{
		message.NewTextMessage(message.RoleUser, "first"),
		message.NewTextMessage(message.RoleAssistant, "second"),
		message.NewTextMessage(message.RoleUser, "third"),
	}
	if got, ok := message.LastByRole(msgs, message.RoleUser); !ok || got.Content.Text() != "third" {
		t.Fatalf("LastByRole user = %+v, %v", got, ok)
	}
	if _, ok := message.LastByRole(msgs, message.RoleSystem); ok {
		t.Fatal("LastByRole system should not match")
	}

	cloned := message.CloneMessages(msgs)
	if len(cloned) != len(msgs) {
		t.Fatalf("CloneMessages length")
	}
	part := cloned[0].Content.Parts[0].(message.TextPart)
	part.Text = "mutated"
	cloned[0].Content.Parts[0] = part
	if msgs[0].Content.Parts[0].(message.TextPart).Text == "mutated" {
		t.Fatal("CloneMessages did not deep-copy")
	}
	if message.CloneMessages(nil) != nil {
		t.Fatal("CloneMessages(nil) must be nil")
	}
}

func TestContentTextSkipsNonTextParts(t *testing.T) {
	call, _ := message.NewCall("c1", "search", map[string]any{"q": "x"})
	c := message.Content{Parts: []message.Part{
		message.TextPart{Text: "hello "},
		message.TextPart{Text: "world"},
		message.ToolCallPart{Call: call},
	}}
	if got := c.Text(); got != "hello world" {
		t.Fatalf("Text() = %q", got)
	}
}

func TestNormalizePartRejectsUnknown(t *testing.T) {
	type bogusPart struct{ message.TextPart }
	var p message.Part = bogusPart{}
	data, err := json.Marshal(message.Content{Parts: []message.Part{p}})
	if err == nil {
		t.Fatalf("expected marshal to reject bogus part, got %s", data)
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error %q should mention unsupported", err)
	}
}
