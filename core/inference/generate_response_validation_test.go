package inference

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func responseValidationRequest(intent Intent) GenerateRequest {
	return GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.NewTextContent("input"),
				Intent:  intent,
			},
		},
	}
}

func responseValidationResponse(parts ...message.Part) GenerateResponse {
	return GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: FinishCompleted,
	}
}

func responseValidationImage(t *testing.T) message.ImagePart {
	t.Helper()
	source, err := media.NewImageBytes([]byte("image"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

func responseValidationAudio(t *testing.T) message.AudioPart {
	t.Helper()
	source, err := media.NewAudioBytes([]byte("audio"), "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	format := media.AudioFormat{
		Encoding: media.AudioEncodingPCM16, SampleRateHz: 16000, Channels: 1,
	}
	return message.AudioPart{Source: source, Format: &format}
}

func responseValidationVideo(t *testing.T) message.VideoPart {
	t.Helper()
	source, err := media.NewVideoBytes([]byte("video"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	return message.VideoPart{Source: source}
}

func responseValidationToolCall(name string) message.Part {
	return message.ToolCallPart{Call: message.ToolCall{
		ID: "call-1", Name: name, Arguments: json.RawMessage(`{}`),
	}}
}

func TestGenerateResponseValidationDetailsNameEveryCheck(t *testing.T) {
	two := 2
	audioFormat := media.AudioFormat{
		Encoding: media.AudioEncodingPCM16, SampleRateHz: 16000, Channels: 1,
	}
	knownTool := message.DefineSchema("known", "known tool").Build()
	otherTool := message.DefineSchema("other", "other tool").Build()
	toolIntent := func(choice ToolChoice) Intent {
		return Intent{Text: &TextIntent{
			Tools:      []message.ToolDefinition{knownTool, otherTool},
			ToolChoice: &choice,
		}}
	}
	textRequest := responseValidationRequest(Intent{Text: &TextIntent{}})

	cases := []struct {
		name     string
		request  GenerateRequest
		response GenerateResponse
		check    generateResponseCheck
		detail   string
		stream   string
	}{
		{
			name:     "message role",
			request:  textRequest,
			response: GenerateResponse{Message: message.Message{Role: message.RoleUser, Content: message.NewTextContent("ok")}, FinishReason: FinishCompleted},
			check:    generateResponseCheckMessageRole,
			detail:   "generate.validation.message_role",
			stream:   "stream.finish.validation.message_role",
		},
		{
			name:     "finish reason",
			request:  textRequest,
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")}, FinishReason: FinishReason("invalid")},
			check:    generateResponseCheckFinishReason,
			detail:   "generate.validation.finish_reason",
			stream:   "stream.finish.validation.finish_reason",
		},
		{
			name:    "empty content",
			request: textRequest,
			response: GenerateResponse{Message: message.Message{
				Role: message.RoleAssistant,
			}, FinishReason: FinishCompleted},
			check:  generateResponseCheckEmptyContent,
			detail: "generate.validation.empty_content",
			stream: "stream.finish.validation.empty_content",
		},
		{
			name:     "message",
			request:  textRequest,
			response: responseValidationResponse(message.ToolResultPart{Result: message.ToolResult{CallID: "call-1", Content: message.NewTextContent("ok")}}),
			check:    generateResponseCheckMessage,
			detail:   "generate.validation.message",
			stream:   "stream.finish.validation.message",
		},
		{
			name:     "usage",
			request:  textRequest,
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")}, FinishReason: FinishCompleted, Usage: Usage{InputTokens: -1}},
			check:    generateResponseCheckUsage,
			detail:   "generate.validation.usage",
			stream:   "stream.finish.validation.usage",
		},
		{
			name:    "provider output",
			request: textRequest,
			response: func() GenerateResponse {
				response := responseValidationResponse(message.TextPart{Text: "ok"})
				response.ProviderOutputs = ProviderOutputs{nil}
				return response
			}(),
			check:  generateResponseCheckProviderOutput,
			detail: "generate.validation.provider_output",
			stream: "stream.finish.validation.provider_output",
		},
		{
			name:     "finish mismatch",
			request:  textRequest,
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")}, FinishReason: FinishToolCalls},
			check:    generateResponseCheckMismatch,
			detail:   "generate.validation.mismatch",
			stream:   "stream.finish.mismatch",
		},
		{
			name:     "part normalization",
			request:  textRequest,
			response: responseValidationResponse(nil),
			check:    generateResponseCheckPart,
			detail:   "generate.validation.part",
			stream:   "stream.finish.validation.part",
		},
		{
			name:     "normalization before invalid media",
			request:  textRequest,
			response: responseValidationResponse(message.ImagePart{}, nil),
			check:    generateResponseCheckPart,
			detail:   "generate.validation.part",
			stream:   "stream.finish.validation.part",
		},
		{
			name:     "unsupported part",
			request:  textRequest,
			response: responseValidationResponse(message.FilePart{URI: "https://example.test/file"}),
			check:    generateResponseCheckUnsupportedPart,
			detail:   "generate.validation.unsupported_part",
			stream:   "stream.finish.validation.unsupported_part",
		},
		{
			name: "malformed tool call",
			request: responseValidationRequest(Intent{Text: &TextIntent{
				Tools: []message.ToolDefinition{knownTool},
			}}),
			response: GenerateResponse{Message: message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: message.ToolCall{ID: "call-1", Name: "known"}}}},
			}, FinishReason: FinishToolCalls},
			check:  generateResponseCheckToolCall,
			detail: "generate.validation.tool_call",
			stream: "stream.finish.tool_call",
		},
		{
			name:     "unrequested text part",
			request:  responseValidationRequest(Intent{Image: &ImageIntent{}}),
			response: responseValidationResponse(message.TextPart{Text: "ok"}),
			check:    generateResponseCheckUnrequestedText,
			detail:   "generate.validation.unrequested_part.text",
			stream:   "stream.finish.validation.unrequested_part.text",
		},
		{
			name:     "unrequested image part",
			request:  textRequest,
			response: responseValidationResponse(responseValidationImage(t)),
			check:    generateResponseCheckUnrequestedImage,
			detail:   "generate.validation.unrequested_part.image",
			stream:   "stream.finish.validation.unrequested_part.image",
		},
		{
			name:     "unrequested audio part",
			request:  textRequest,
			response: responseValidationResponse(responseValidationAudio(t)),
			check:    generateResponseCheckUnrequestedAudio,
			detail:   "generate.validation.unrequested_part.audio",
			stream:   "stream.finish.validation.unrequested_part.audio",
		},
		{
			name:     "unrequested video part",
			request:  textRequest,
			response: responseValidationResponse(responseValidationVideo(t)),
			check:    generateResponseCheckUnrequestedVideo,
			detail:   "generate.validation.unrequested_part.video",
			stream:   "stream.finish.validation.unrequested_part.video",
		},
		{
			name:     "image media",
			request:  responseValidationRequest(Intent{Image: &ImageIntent{Delivery: media.SourceURL}}),
			response: responseValidationResponse(responseValidationImage(t)),
			check:    generateResponseCheckImageMedia,
			detail:   "generate.validation.media.image",
			stream:   "stream.finish.validation.media.image",
		},
		{
			name:     "audio media",
			request:  responseValidationRequest(Intent{Audio: &AudioIntent{Format: audioFormat}}),
			response: responseValidationResponse(message.AudioPart{}),
			check:    generateResponseCheckAudioMedia,
			detail:   "generate.validation.media.audio",
			stream:   "stream.finish.validation.media.audio",
		},
		{
			name:     "video media",
			request:  responseValidationRequest(Intent{Video: &VideoIntent{}}),
			response: responseValidationResponse(message.VideoPart{}),
			check:    generateResponseCheckVideoMedia,
			detail:   "generate.validation.media.video",
			stream:   "stream.finish.validation.media.video",
		},
		{
			name:     "unrequested tool",
			request:  textRequest,
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{responseValidationToolCall("known")}}}, FinishReason: FinishToolCalls},
			check:    generateResponseCheckUnrequestedTool,
			detail:   "generate.validation.unrequested_tool",
			stream:   "stream.finish.validation.unrequested_tool",
		},
		{
			name: "undefined tool",
			request: responseValidationRequest(Intent{Text: &TextIntent{
				Tools: []message.ToolDefinition{knownTool},
			}}),
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{responseValidationToolCall("ghost")}}}, FinishReason: FinishToolCalls},
			check:    generateResponseCheckUndefinedTool,
			detail:   "generate.validation.undefined_tool",
			stream:   "stream.finish.undefined_tool",
		},
		{
			name:     "tool choice none",
			request:  responseValidationRequest(toolIntent(ToolChoice{Kind: ToolChoiceNone})),
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{responseValidationToolCall("known")}}}, FinishReason: FinishToolCalls},
			check:    generateResponseCheckToolChoice,
			detail:   "generate.validation.tool_choice",
			stream:   "stream.finish.validation.tool_choice",
		},
		{
			name:     "tool choice required",
			request:  responseValidationRequest(toolIntent(ToolChoice{Kind: ToolChoiceRequired})),
			response: responseValidationResponse(message.TextPart{Text: "ok"}),
			check:    generateResponseCheckToolChoice,
			detail:   "generate.validation.tool_choice",
			stream:   "stream.finish.validation.tool_choice",
		},
		{
			name:     "tool choice named",
			request:  responseValidationRequest(toolIntent(ToolChoice{Kind: ToolChoiceNamed, Name: "known"})),
			response: GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{responseValidationToolCall("other")}}}, FinishReason: FinishToolCalls},
			check:    generateResponseCheckToolChoice,
			detail:   "generate.validation.tool_choice",
			stream:   "stream.finish.validation.tool_choice",
		},
		{
			name:     "no text",
			request:  textRequest,
			response: responseValidationResponse(message.ReasoningPart{Text: "thinking", Signature: "sig"}),
			check:    generateResponseCheckNoText,
			detail:   "generate.validation.no_text",
			stream:   "stream.finish.validation.no_text",
		},
		{
			name:     "text format",
			request:  responseValidationRequest(Intent{Text: &TextIntent{Response: &ResponseFormat{Kind: ResponseJSONObject}}}),
			response: responseValidationResponse(message.TextPart{Text: "not json"}),
			check:    generateResponseCheckText,
			detail:   "generate.validation.text",
			stream:   "stream.finish.validation.text",
		},
		{
			name:     "image count",
			request:  responseValidationRequest(Intent{Image: &ImageIntent{Count: &two}}),
			response: responseValidationResponse(responseValidationImage(t)),
			check:    generateResponseCheckImageCount,
			detail:   "generate.validation.count.image",
			stream:   "stream.finish.validation.count.image",
		},
		{
			name:     "audio count",
			request:  responseValidationRequest(Intent{Audio: &AudioIntent{Format: audioFormat, Count: &two}}),
			response: responseValidationResponse(responseValidationAudio(t)),
			check:    generateResponseCheckAudioCount,
			detail:   "generate.validation.count.audio",
			stream:   "stream.finish.validation.count.audio",
		},
		{
			name:     "video count",
			request:  responseValidationRequest(Intent{Video: &VideoIntent{}}),
			response: responseValidationResponse(message.ReasoningPart{Text: "thinking", Signature: "sig"}),
			check:    generateResponseCheckVideoCount,
			detail:   "generate.validation.count.video",
			stream:   "stream.finish.validation.count.video",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.request.Validate(); err != nil {
				t.Fatalf("request Validate: %v", err)
			}
			err := tc.response.ValidateFor(tc.request)
			if err == nil {
				t.Fatal("ValidateFor succeeded, want validation error")
			}
			var checkErr *generateResponseCheckError
			if !errors.As(err, &checkErr) {
				t.Fatalf("ValidateFor error = %T %v, want generateResponseCheckError", err, err)
			}
			if checkErr.check == "" {
				t.Fatal("validation error must name a nonempty check")
			}
			if checkErr.check != tc.check {
				t.Fatalf("check = %q, want %q", checkErr.check, tc.check)
			}
			wantKind := InvalidProviderResponse
			if tc.check == generateResponseCheckUndefinedTool {
				wantKind = UndefinedTool
			}
			for _, scope := range []struct {
				name   string
				detail string
			}{
				{name: "generate", detail: tc.detail},
				{name: "stream.finish", detail: tc.stream},
			} {
				wrapped := newResponseValidationError(OperationGenerate, scope.name, err)
				if wrapped.Detail != scope.detail {
					t.Fatalf("%s Detail = %q, want %q", scope.name, wrapped.Detail, scope.detail)
				}
				if wrapped.Kind != wantKind {
					t.Fatalf("%s Kind = %q, want %q", scope.name, wrapped.Kind, wantKind)
				}
				if !errors.Is(wrapped, err) {
					t.Fatalf("%s lost original validation error", scope.name)
				}
			}
		})
	}
}

func TestResponseValidationDetailUsesScope(t *testing.T) {
	compile := GenerateCompiler[string](func(
		_ context.Context,
		_ ModelRef,
		request GenerateRequest,
		shape GenerateExecutionShape,
	) (Compiled[string], error) {
		fields := request.ActiveFieldsFor(shape)
		decisions := make([]Decision, len(fields))
		for index, field := range fields {
			decisions[index] = Decision{Field: field, Disposition: Native}
		}
		return Compiled[string]{
			Wire: "wire",
			Report: CompileReport{
				Operation: OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	})
	driver, err := BindGenerate(
		compile,
		Transport[string, string](func(context.Context, string) (string, error) {
			return "raw", nil
		}),
		Decoder[string, GenerateResponse](func(context.Context, string) (GenerateResponse, error) {
			return responseValidationResponse(message.ReasoningPart{Text: "thinking", Signature: "sig"}), nil
		}),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	request := responseValidationRequest(Intent{Text: &TextIntent{}})
	_, err = driver.Execute(context.Background(), ModelRef{
		ID: ModelID{Provider: "fake", Name: "model-1"},
	}, request)
	if err == nil {
		t.Fatal("Execute succeeded, want validation error")
	}
	var responseErr *Error
	if !errors.As(err, &responseErr) {
		t.Fatalf("Execute error = %T %v, want *Error", err, err)
	}
	if responseErr.Kind != InvalidProviderResponse {
		t.Fatalf("Kind = %q, want %q", responseErr.Kind, InvalidProviderResponse)
	}
	if responseErr.Operation != OperationGenerate {
		t.Fatalf("Operation = %q, want %q", responseErr.Operation, OperationGenerate)
	}
	if responseErr.Detail != "generate.validation.no_text" {
		t.Fatalf("Detail = %q, want generate.validation.no_text", responseErr.Detail)
	}
	if got := responseErr.Error(); got != "invalid_provider_response during generate: generate.validation.no_text" {
		t.Fatalf("Error() = %q", got)
	}

	stream := detailTestStreamWithMeta([]GenerateStreamEvent{
		{PartIndex: 0, Delta: ReasoningDelta{Text: "thinking", Signature: "sig"}},
		{FinishReason: FinishCompleted},
	}, request, "req-1", "resp-1")
	streamErr := nextStreamError(t, stream)
	if streamErr.Detail != "stream.finish.validation.no_text" {
		t.Fatalf("stream Detail = %q, want stream.finish.validation.no_text", streamErr.Detail)
	}
	if streamErr.Operation != responseErr.Operation || streamErr.Kind != responseErr.Kind {
		t.Fatalf("stream classification = %q/%q, want %q/%q",
			streamErr.Operation, streamErr.Kind, responseErr.Operation, responseErr.Kind)
	}
	if streamErr.RequestID != "req-1" {
		t.Fatalf("stream RequestID = %q, want req-1", streamErr.RequestID)
	}
	var checkErr *generateResponseCheckError
	if !errors.As(streamErr, &checkErr) || checkErr.check != generateResponseCheckNoText {
		t.Fatalf("stream cause = %v, want no_text check", streamErr)
	}
	_, resultErr := stream.Result()
	if !errors.Is(resultErr, streamErr) {
		t.Fatalf("Result error = %v, want original terminal error", resultErr)
	}
}
