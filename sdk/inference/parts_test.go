package inference

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func TestContentRoundTripAcrossGenerateAndEmbed(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	parts := Content{Parts: []Part{TextPart{Text: "describe"}, ImagePart{Source: image}}}

	req := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: parts,
				Intent:  Intent{Text: &TextIntent{}},
			},
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("GenerateRequest.Validate: %v", err)
	}
	if err := (EmbedRequest{Items: []EmbedItem{{Content: parts}}}).Validate(); err != nil {
		t.Fatalf("EmbedRequest.Validate: %v", err)
	}

	data, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Content
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Parts) != 2 ||
		decoded.Parts[0].Kind() != PartText ||
		decoded.Parts[1].Kind() != PartImage {
		t.Fatalf("decoded parts = %#v", decoded)
	}
}

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
	call, err := tool.NewCall("call-1", "search", map[string]any{"query": "cat"})
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	content := Content{Parts: []Part{TextPart{Text: "hello"},
		ImagePart{Source: image},
		AudioPart{Source: audio},
		VideoPart{Source: video},
		FilePart{URI: "s3://bucket/document.pdf", MediaType: "application/pdf", Name: "document.pdf"},
		DataPart{MediaType: "application/json", Value: json.RawMessage(`{"answer":42}`)},
		ToolCallPart{Call: call},
		ToolResultPart{Result: tool.Result{CallID: "call-1", Content: "found"}},
		ReasoningPart{Text: "let me think", Signature: "sig-1"}},
	}

	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Content
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantKinds := []PartKind{
		PartText, PartImage, PartAudio, PartVideo, PartFile, PartData,
		PartToolCall, PartToolResult, PartReasoning,
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
	clone.Parts[5].(DataPart).Value[0] = '['
	clone.Parts[6].(ToolCallPart).Call.Arguments[0] = '['
	if string(decoded.Parts[5].(DataPart).Value) != `{"answer":42}` {
		t.Fatal("data part clone mutated source")
	}
	if string(decoded.Parts[6].(ToolCallPart).Call.Arguments) != `{"query":"cat"}` {
		t.Fatal("tool call part clone mutated source")
	}
}

func TestPartValidationLeavesEmbedSupportToCompiler(t *testing.T) {
	audio, err := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	if err := (EmbedItem{Content: Content{Parts: []Part{AudioPart{Source: audio}}}}).Validate(); err != nil {
		t.Fatalf("EmbedItem.Validate rejected canonical audio before compile: %v", err)
	}
	if err := (Message{
		Role: RoleUser,
		Content: Content{Parts: []Part{ToolCallPart{Call: tool.Call{
			ID: "call-1", Name: "search", Arguments: json.RawMessage(`{}`),
		}}}},
	}).Validate(); err == nil {
		t.Fatal("user message must reject tool call parts")
	}
}

func TestReasoningPartValidationAndRoles(t *testing.T) {
	if err := (ReasoningPart{}).Validate(); err == nil {
		t.Fatal("empty reasoning part must be invalid")
	}
	if err := (ReasoningPart{Signature: "sig"}).Validate(); err != nil {
		t.Fatalf("redacted reasoning (signature only) must be valid: %v", err)
	}
	if err := (ReasoningPart{Text: "thinking"}).Validate(); err != nil {
		t.Fatalf("unsigned reasoning must be valid: %v", err)
	}

	reasoning := Content{Parts: []Part{ReasoningPart{Text: "t", Signature: "s"}}}
	if err := (Message{Role: RoleAssistant, Content: reasoning}).Validate(); err != nil {
		t.Fatalf("assistant message must accept reasoning: %v", err)
	}
	for _, role := range []Role{RoleSystem, RoleUser, RoleTool} {
		if err := (Message{Role: role, Content: reasoning}).Validate(); err == nil {
			t.Fatalf("%s message must reject reasoning parts", role)
		}
	}
}

func TestReasoningPartRoundTripsSignature(t *testing.T) {
	content := Content{Parts: []Part{
		ReasoningPart{Text: "visible", Signature: "sig-1"},
		ReasoningPart{Signature: "opaque-redacted-data"},
	}}
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Content
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	first := decoded.Parts[0].(ReasoningPart)
	if first.Text != "visible" || first.Signature != "sig-1" {
		t.Fatalf("signed reasoning round-trip = %#v", first)
	}
	second := decoded.Parts[1].(ReasoningPart)
	if second.Text != "" || second.Signature != "opaque-redacted-data" {
		t.Fatalf("redacted reasoning round-trip = %#v", second)
	}
}
