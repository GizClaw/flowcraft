package inference_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/goccy/go-json"
)

// A response may only carry the modality the request asked for. These pin the
// boundary that used to be implicit: a driver that answered an image request
// with text, or a video request with an image, must fail as a provider bug
// rather than reach a caller that cannot use the payload.

func outputRequest(t *testing.T, output inference.Output, tools ...message.ToolDefinition) inference.GenerateRequest {
	t.Helper()
	request := textRequest(t)
	request.Output = output
	if len(tools) > 0 {
		request.Tools = mustToolset(t, tools...)
	}
	return request
}

func imagePart(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("image source: %v", err)
	}
	return message.ImagePart{Source: source}
}

func audioPart(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewAudioURL("https://example.com/a.mp3", "audio/mpeg")
	if err != nil {
		t.Fatalf("audio source: %v", err)
	}
	return message.AudioPart{Source: source}
}

func videoPart(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewVideoURL("https://example.com/a.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("video source: %v", err)
	}
	return message.VideoPart{Source: source}
}

func partResponse(part message.Part, finish inference.FinishReason) inference.GenerateResponse {
	return partsResponse([]message.Part{part}, finish)
}

func partsResponse(parts []message.Part, finish inference.FinishReason) inference.GenerateResponse {
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: finish,
	}
}

func TestGenerateResponseValidateForChecksOutputModality(t *testing.T) {
	toolCall := message.ToolCallPart{Call: message.ToolCall{
		ID:        "c1",
		Name:      "search",
		Arguments: message.NewArguments(json.RawMessage(`{}`)),
	}}

	tests := []struct {
		name       string
		request    inference.GenerateRequest
		response   inference.GenerateResponse
		wantKind   inference.ErrorKind
		wantDetail string
	}{
		{
			name:     "text accepts text",
			request:  outputRequest(t, inference.OutputText),
			response: partResponse(message.TextPart{Text: "ok"}, inference.FinishStop),
		},
		{
			name:    "text accepts reasoning beside text",
			request: outputRequest(t, inference.OutputText),
			response: partsResponse([]message.Part{
				message.ReasoningPart{Text: "thinking"},
				message.TextPart{Text: "ok"},
			}, inference.FinishStop),
		},
		{
			name:       "text rejects image",
			request:    outputRequest(t, inference.OutputText),
			response:   partResponse(imagePart(t), inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.image",
		},
		{
			name:       "text rejects audio",
			request:    outputRequest(t, inference.OutputText),
			response:   partResponse(audioPart(t), inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.audio",
		},
		{
			name:       "text rejects video",
			request:    outputRequest(t, inference.OutputText),
			response:   partResponse(videoPart(t), inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.video",
		},
		{
			name:     "image accepts image",
			request:  outputRequest(t, inference.OutputImage),
			response: partResponse(imagePart(t), inference.FinishStop),
		},
		{
			name:    "image accepts auxiliary data beside image",
			request: outputRequest(t, inference.OutputImage),
			response: partsResponse([]message.Part{
				imagePart(t),
				message.DataPart{MediaType: "application/json", Value: json.RawMessage(`{}`)},
			}, inference.FinishStop),
		},
		{
			name:       "data alone is not the requested answer",
			request:    outputRequest(t, inference.OutputVideo),
			response:   partResponse(message.DataPart{MediaType: "application/json", Value: json.RawMessage(`{}`)}, inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.missing_part.video",
		},
		{
			name:       "image rejects text",
			request:    outputRequest(t, inference.OutputImage),
			response:   partResponse(message.TextPart{Text: "ok"}, inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.text",
		},
		{
			name:     "audio accepts audio",
			request:  outputRequest(t, inference.OutputAudio),
			response: partResponse(audioPart(t), inference.FinishStop),
		},
		{
			name:       "audio rejects video",
			request:    outputRequest(t, inference.OutputAudio),
			response:   partResponse(videoPart(t), inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.video",
		},
		{
			name:     "video accepts video",
			request:  outputRequest(t, inference.OutputVideo),
			response: partResponse(videoPart(t), inference.FinishStop),
		},
		{
			name:       "video rejects image",
			request:    outputRequest(t, inference.OutputVideo),
			response:   partResponse(imagePart(t), inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.image",
		},
		{
			name:       "video rejects text",
			request:    outputRequest(t, inference.OutputVideo),
			response:   partResponse(message.TextPart{Text: "ok"}, inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.text",
		},
		{
			name:     "text tool call is not another modality",
			request:  outputRequest(t, inference.OutputText, searchTool()),
			response: partResponse(toolCall, inference.FinishToolCalls),
		},
		{
			name:       "text tool call without text does not finish",
			request:    outputRequest(t, inference.OutputText),
			response:   partResponse(toolCall, inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.missing_part.text",
		},
		{
			name:       "reasoning alone does not finish text",
			request:    outputRequest(t, inference.OutputText),
			response:   partResponse(message.ReasoningPart{Text: "thinking"}, inference.FinishStop),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.missing_part.text",
		},
		{
			name:     "content filter may close with the requested modality",
			request:  outputRequest(t, inference.OutputVideo),
			response: partResponse(videoPart(t), inference.FinishContentFilter),
		},
		{
			name:       "content filter does not excuse another modality",
			request:    outputRequest(t, inference.OutputVideo),
			response:   partResponse(message.TextPart{Text: "blocked"}, inference.FinishContentFilter),
			wantKind:   inference.ProviderResponseUnusable,
			wantDetail: "generate.validation.unrequested_part.text",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.response.ValidateFor(test.request)
			if test.wantKind == "" {
				if err != nil {
					t.Fatalf("ValidateFor = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateFor = nil, want a rejection")
			}
			if !inference.IsKind(err, test.wantKind) {
				t.Fatalf("kind = %v, want %v", err, test.wantKind)
			}
			if got := asInferenceError(t, err).Detail; got != test.wantDetail {
				t.Fatalf("detail = %q, want %q", got, test.wantDetail)
			}
		})
	}
}
