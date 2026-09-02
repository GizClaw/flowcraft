package inference

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestIntentOutputKinds(t *testing.T) {
	if kinds := (Intent{}).OutputKinds(); len(kinds) != 0 {
		t.Fatalf("empty intent kinds = %v, want none", kinds)
	}
	if kinds := (Intent{Text: &TextIntent{}}).OutputKinds(); !reflect.DeepEqual(
		kinds,
		[]message.PartKind{message.PartText},
	) {
		t.Fatalf("text intent kinds = %v", kinds)
	}
	all := Intent{
		Text:  &TextIntent{},
		Image: &ImageIntent{},
		Audio: &AudioIntent{},
		Video: &VideoIntent{},
	}
	want := []message.PartKind{
		message.PartText,
		message.PartImage,
		message.PartAudio,
		message.PartVideo,
	}
	if kinds := all.OutputKinds(); !reflect.DeepEqual(kinds, want) {
		t.Fatalf("all intent kinds = %v, want %v", kinds, want)
	}
}

func TestGenerateRequestModelHintCloneValidateAndJSON(t *testing.T) {
	request := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
				Intent: Intent{Text: &TextIntent{}},
			},
		},
		ModelHint: "good/model-1",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if clone := request.Clone(); clone.ModelHint != request.ModelHint {
		t.Fatalf("Clone().ModelHint = %q, want %q", clone.ModelHint, request.ModelHint)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded GenerateRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ModelHint != request.ModelHint {
		t.Fatalf("JSON round-trip ModelHint = %q, want %q", decoded.ModelHint, request.ModelHint)
	}
}

func TestGenerateRequestRequestMetadataCloneAndJSON(t *testing.T) {
	request := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
				Intent: Intent{Text: &TextIntent{}},
			},
		},
		RequestMetadata: map[string]string{
			"session_id": "s-1",
			"turn_id":    "turn-1",
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	clone := request.Clone()
	clone.RequestMetadata["session_id"] = "mutated"
	if request.RequestMetadata["session_id"] != "s-1" {
		t.Fatalf(
			"clone aliases RequestMetadata: source session_id = %q, want s-1",
			request.RequestMetadata["session_id"],
		)
	}
	if clone.RequestMetadata["turn_id"] != "turn-1" {
		t.Fatalf("Clone().RequestMetadata turn_id = %q, want turn-1", clone.RequestMetadata["turn_id"])
	}

	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded GenerateRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.RequestMetadata, request.RequestMetadata) {
		t.Fatalf(
			"JSON round-trip RequestMetadata = %v, want %v",
			decoded.RequestMetadata,
			request.RequestMetadata,
		)
	}
	sawLedger := false
	for _, field := range request.ActiveFieldsFor(GenerateExecutionUnary) {
		if field == FieldGenerateRequestMetadata {
			sawLedger = true
			break
		}
	}
	if !sawLedger {
		t.Fatal("RequestMetadata did not enter ActiveFields")
	}
}

func TestValidateGenerateText(t *testing.T) {
	arraySchema := json.RawMessage(
		`{"type":"array","items":{"type":"string"}}`,
	)
	cases := []struct {
		name    string
		output  string
		format  *ResponseFormat
		wantErr string
	}{
		{
			name:   "no format skips validation",
			output: `["a","b"]`,
		},
		{
			name:   "text format skips validation",
			output: `["a","b"]`,
			format: &ResponseFormat{Kind: ResponseText},
		},
		{
			name:   "json_object accepts object",
			output: `{"answer":"42"}`,
			format: &ResponseFormat{Kind: ResponseJSONObject},
		},
		{
			name:    "json_object rejects array",
			output:  `["a","b"]`,
			format:  &ResponseFormat{Kind: ResponseJSONObject},
			wantErr: "structured generate response must be a JSON object",
		},
		{
			name:   "json_schema accepts array root",
			output: `["a","b"]`,
			format: &ResponseFormat{
				Kind:   ResponseJSONSchema,
				Name:   "answer",
				Schema: arraySchema,
			},
		},
		{
			name:    "json_schema array root rejects object",
			output:  `{"a":"b"}`,
			format:  &ResponseFormat{Kind: ResponseJSONSchema, Name: "answer", Schema: arraySchema},
			wantErr: "does not match requested JSON schema",
		},
		{
			name:   "json_schema rejects non-conforming object",
			output: `{"answer":42}`,
			format: &ResponseFormat{
				Kind: ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`,
				),
			},
			wantErr: "does not match requested JSON schema",
		},
		{
			name:    "json_schema rejects invalid JSON",
			output:  "not json",
			format:  &ResponseFormat{Kind: ResponseJSONSchema, Name: "answer", Schema: arraySchema},
			wantErr: "structured generate response is not valid JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.format != nil {
				if err := tc.format.Validate(); err != nil {
					t.Fatalf("format Validate: %v", err)
				}
			}
			err := validateGenerateText(tc.output, tc.format)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGenerateText = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateGenerateText error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateForUndefinedTool(t *testing.T) {
	req := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
				Intent: Intent{Text: &TextIntent{
					Tools: []message.ToolDefinition{message.DefineSchema("known", "a known tool").Build()},
				}},
			},
		},
	}
	resp := GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ToolCallPart{Call: message.ToolCall{
					ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`),
				}},
			}},
		},
		FinishReason: FinishToolCalls,
	}
	err := resp.ValidateFor(req)
	var ute *undefinedToolError
	if !errors.As(err, &ute) {
		t.Fatalf("ValidateFor error = %v, want *undefinedToolError", err)
	}
	if ute.Index != 0 || ute.Call.Name != "ghost" {
		t.Fatalf("undefined tool error = %+v, want call 0 ghost", ute)
	}

	wrapped := newResponseValidationError(OperationGenerate, err)
	if wrapped.Kind != UndefinedTool {
		t.Fatalf("wrapped kind = %q, want %q", wrapped.Kind, UndefinedTool)
	}
	if wrapped.UndefinedToolCall == nil ||
		wrapped.UndefinedToolCall.ID != "call-1" ||
		wrapped.UndefinedToolCall.Name != "ghost" {
		t.Fatalf("wrapped call = %+v, want call-1/ghost", wrapped.UndefinedToolCall)
	}
	if !errdefs.IsValidation(wrapped) {
		t.Fatalf("wrapped error must classify as validation, got %v", wrapped)
	}
}

func TestValidateForKnownToolCall(t *testing.T) {
	req := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
				Intent: Intent{Text: &TextIntent{
					Tools: []message.ToolDefinition{message.DefineSchema("known", "a known tool").Build()},
				}},
			},
		},
	}
	resp := GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ToolCallPart{Call: message.ToolCall{
					ID: "call-1", Name: "known", Arguments: json.RawMessage(`{}`),
				}},
			}},
		},
		FinishReason: FinishToolCalls,
	}
	if err := resp.ValidateFor(req); err != nil {
		t.Fatalf("ValidateFor known tool call: %v", err)
	}
}
