package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// These tests compile one content shape at a time. The mapping from a canonical
// part to the SDK's block type is where a silently dropped or mis-typed part would
// live, so each shape is pinned rather than inferred from one combined request.

func compileRequest(t *testing.T, context []message.Message, input message.Content) inference.GenerateRequest {
	t.Helper()
	if input.Parts == nil {
		input = message.NewTextContent("next")
	}
	request := inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "claude-3-5-sonnet"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: input,
		},
		Context: context,
		Output:  inference.OutputText,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

// compiledJSON compiles the request and marshals the result: the SDK's marshaller is
// what the provider actually receives, so an assertion on its output is an assertion
// on the wire shape rather than on a private field.
func compiledJSON(t *testing.T, request inference.GenerateRequest) string {
	t.Helper()
	compiled, err := testDriver(Settings{APIKey: "key"}).compile(context.Background(), request, pipeline.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	payload, err := json.Marshal(compiled.Wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(payload)
}

// TestCompileReasoningRoundTrips covers both reasoning shapes: a signed block keeps
// its text and signature, and a signature with no text is the redacted form the
// provider replays verbatim.
func TestCompileReasoningRoundTrips(t *testing.T) {
	request := compileRequest(t, []message.Message{
		{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ReasoningPart{Text: "thinking", Signature: "sig-1"},
			message.ReasoningPart{Signature: "sig-2"},
			message.TextPart{Text: "the answer"},
		}}},
	}, message.Content{})

	text := compiledJSON(t, request)
	for _, want := range []string{`"thinking"`, `"sig-1"`, `"sig-2"`, "redacted_thinking", `"the answer"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("compiled messages are missing %s: %s", want, text)
		}
	}
}

// TestCompileToolResultsFlattensTextAndKeepsImages pins the two tool-result shapes:
// a plain text result is flattened into the block's text, and one carrying an image
// keeps the structured content array.
func TestCompileToolResultsFlattensTextAndKeepsImages(t *testing.T) {
	image, err := media.NewImageBytes([]byte("PNGDATA"), "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}

	textResult := compileRequest(t, []message.Message{{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID: "call-1",
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "found it"},
				}},
			}},
		}},
	}}, message.Content{})
	text := compiledJSON(t, textResult)
	if !strings.Contains(text, "found it") || !strings.Contains(text, "call-1") {
		t.Fatalf("the tool result was lost: %s", text)
	}

	imageResult := compileRequest(t, []message.Message{{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID: "call-2",
				Content: message.Content{Parts: []message.Part{
					message.ImagePart{Source: image},
				}},
			}},
		}},
	}}, message.Content{})
	if got := compiledJSON(t, imageResult); !strings.Contains(got, "image") {
		t.Fatalf("an image tool result lost its block: %s", got)
	}
}

// TestCompileImagePartIsBase64 is the shape a screenshot arrives in: bytes on the
// wire, base64 in the request, and the media type preserved so the provider knows
// what it is looking at.
func TestCompileImagePartIsBase64(t *testing.T) {
	image, err := media.NewImageBytes([]byte("hello-image"), "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	request := compileRequest(t, nil, message.Content{Parts: []message.Part{
		message.ImagePart{Source: image},
	}})

	text := compiledJSON(t, request)
	if !strings.Contains(text, media.Base64([]byte("hello-image"))) {
		t.Fatalf("the image bytes were not base64-encoded: %s", text)
	}
	if !strings.Contains(text, "image/png") {
		t.Fatalf("the media type was lost: %s", text)
	}
}

// TestCompileToolCallHistoryKeepsArguments covers the assistant turn that asked for a
// tool: the arguments are decoded from their JSON text, and an unparseable body
// degrades to an empty object rather than failing the whole request.
func TestCompileToolCallHistoryKeepsArguments(t *testing.T) {
	request := compileRequest(t, []message.Message{
		{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: message.ToolCall{
				ID: "call-3", Name: "search",
				Arguments: message.NewArguments(json.RawMessage(`{"q":"x"}`)),
			}},
			message.ToolCallPart{Call: message.ToolCall{
				ID: "call-4", Name: "search",
				Arguments: message.NewArguments(json.RawMessage(`{"q":"y"}`)),
			}},
		}}},
		// Every call needs its answer before the turn is a valid history.
		{Role: message.RoleTool, Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID: "call-3", Content: message.NewTextContent("one"),
			}},
			message.ToolResultPart{Result: message.ToolResult{
				CallID: "call-4", Content: message.NewTextContent("two"),
			}},
		}}},
	}, message.Content{})

	text := compiledJSON(t, request)
	if !strings.Contains(text, `"q"`) || !strings.Contains(text, `"search"`) {
		t.Fatalf("the tool call arguments were lost: %s", text)
	}
}

// TestArgsValueDegradesToAnEmptyObject covers the decode that keeps a replayed
// history from failing: the arguments are JSON text on the wire, and a body this
// build cannot parse becomes an empty object rather than an error. A caller cannot
// reach it through a validated request — validation rejects malformed arguments —
// so it is pinned here, where the provider's own history would.
func TestArgsValueDegradesToAnEmptyObject(t *testing.T) {
	if got := argsValue(nil); got == nil {
		t.Fatal("argsValue(nil) = nil, want an empty object")
	}
	if got := argsValue([]byte("not json")); got == nil {
		t.Fatal("argsValue(malformed) = nil, want an empty object")
	}
	decoded, ok := argsValue([]byte(`{"q":"x"}`)).(map[string]any)
	if !ok || decoded["q"] != "x" {
		t.Fatalf("argsValue(valid) = %v", argsValue([]byte(`{"q":"x"}`)))
	}
}
