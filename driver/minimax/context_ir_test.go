package minimax

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func contextIRRequest(
	parts []message.Part,
	options ContextIROptions,
) inference.GenerateRequest {
	var extensions inference.Extensions
	extensions = append(extensions, options)
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: parts},
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
		Extensions: extensions,
	}
}

func compileContextIRWire(
	t *testing.T,
	model string,
	request inference.GenerateRequest,
) (contextIRWire, inference.CompileReport, error) {
	t.Helper()
	entry, ok := catalog[model]
	if !ok {
		t.Fatalf("catalog model %q missing", model)
	}
	compiled, err := compileContextIR(wireModel(model, entry), entry)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: driverID, Name: model}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return contextIRWire{}, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

func contextIRField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(ContextIROptions{})
}

func TestCompileContextIRDefaults(t *testing.T) {
	request := contextIRRequest(
		[]message.Part{message.TextPart{Text: "a boy playing basketball"}},
		ContextIROptions{},
	)
	wire, report, err := compileContextIRWire(t, "MiniMax-H3-Context-IR", request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.model != "MiniMax-H3" {
		t.Fatalf("wire.model = %q, want MiniMax-H3", wire.model)
	}
	if wire.prompt != "a boy playing basketball" {
		t.Fatalf("wire.prompt = %q", wire.prompt)
	}
	if wire.duration != 6 {
		t.Fatalf("wire.duration = %d, want 6", wire.duration)
	}
	if wire.ratio != "16:9" {
		t.Fatalf("wire.ratio = %q, want 16:9", wire.ratio)
	}
}

func TestCompileContextIROptions(t *testing.T) {
	fiveSeconds := int64(5_000)
	request := contextIRRequest(
		[]message.Part{message.TextPart{Text: "a cinematic scene"}},
		ContextIROptions{
			DurationMillis: &fiveSeconds,
			Ratio:          "21:9",
			CallbackURL:    "https://example.com/callback",
		},
	)
	wire, report, err := compileContextIRWire(t, "MiniMax-H3-Context-IR", request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.duration != 5 || wire.ratio != "21:9" ||
		wire.callbackURL != "https://example.com/callback" {
		t.Fatalf("wire = %+v", wire)
	}
}

func TestCompileContextIRGatesOptions(t *testing.T) {
	threeSeconds := int64(3_000)
	sixteenSeconds := int64(16_000)
	fractional := int64(1_500)
	adaptive := "adaptive"

	cases := []struct {
		name    string
		options ContextIROptions
		parts   []message.Part
		field   inference.FieldID
		reason  string
	}{
		{
			name:    "duration below minimum",
			options: ContextIROptions{DurationMillis: &threeSeconds},
			field:   contextIRField("duration_millis"),
			reason:  "4-15s",
		},
		{
			name:    "duration above maximum",
			options: ContextIROptions{DurationMillis: &sixteenSeconds},
			field:   contextIRField("duration_millis"),
			reason:  "4-15s",
		},
		{
			name:    "fractional duration",
			options: ContextIROptions{DurationMillis: &fractional},
			field:   contextIRField("duration_millis"),
			reason:  "whole seconds",
		},
		{
			name:    "ratio outside official set",
			options: ContextIROptions{Ratio: "2:1"},
			field:   contextIRField("ratio"),
			reason:  "ratios are adaptive",
		},
		{
			name:    "adaptive ratio on text-only task",
			options: ContextIROptions{Ratio: adaptive},
			field:   contextIRField("ratio"),
			reason:  "explicit ratio",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := tc.parts
			if parts == nil {
				parts = []message.Part{message.TextPart{Text: "a cinematic scene"}}
			}
			_, report, err := compileContextIRWire(
				t,
				"MiniMax-H3-Context-IR",
				contextIRRequest(parts, tc.options),
			)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q not rejected; report = %+v", tc.field, report)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}

	t.Run("adaptive ratio accepted with image input", func(t *testing.T) {
		wire, report, err := compileContextIRWire(
			t,
			"MiniMax-H3-Context-IR",
			contextIRRequest(
				[]message.Part{
					message.TextPart{Text: "a scene"},
					videoImagePart(t, "https://example.com/a.png"),
				},
				ContextIROptions{Ratio: adaptive},
			),
		)
		if err != nil {
			t.Fatalf("compile: %v; report = %+v", err, report)
		}
		if wire.ratio != "adaptive" || wire.firstFrame != "https://example.com/a.png" {
			t.Fatalf("wire = %+v", wire)
		}
	})
}

func TestCompileContextIRInputs(t *testing.T) {
	rejected := []struct {
		name   string
		parts  []message.Part
		field  inference.FieldID
		reason string
	}{
		{
			name: "first/last frame mixed with reference inputs",
			parts: []message.Part{
				videoImagePart(t, "https://example.com/a.png"),
				videoImagePart(t, "https://example.com/b.png"),
				videoClipPart(t, "https://example.com/clip.mp4"),
			},
			field:  inference.FieldGenerateInputImage,
			reason: "mutually exclusive",
		},
		{
			name: "too many reference images",
			parts: func() []message.Part {
				parts := make([]message.Part, 0, 11)
				for i := 0; i < 10; i++ {
					parts = append(parts, videoImagePart(t, "https://example.com/x.png"))
				}
				return parts
			}(),
			field:  inference.FieldGenerateInputImage,
			reason: "at most 9",
		},
		{
			name:   "missing text prompt",
			parts:  []message.Part{},
			field:  inference.FieldGenerateIntentText,
			reason: "non-empty text prompt",
		},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			parts := tc.parts
			if parts == nil {
				parts = []message.Part{}
			}
			_, report, err := compileContextIRWire(
				t,
				"MiniMax-H3-Context-IR",
				contextIRRequest(parts, ContextIROptions{}),
			)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q not rejected; report = %+v", tc.field, report)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}

	t.Run("first and last frame map onto content", func(t *testing.T) {
		wire, report, err := compileContextIRWire(
			t,
			"MiniMax-H3-Context-IR",
			contextIRRequest(
				[]message.Part{
					message.TextPart{Text: "a scene"},
					videoImagePart(t, "https://example.com/a.png"),
					videoImagePart(t, "https://example.com/b.png"),
				},
				ContextIROptions{},
			),
		)
		if err != nil {
			t.Fatalf("compile: %v; report = %+v", err, report)
		}
		if wire.firstFrame != "https://example.com/a.png" ||
			wire.lastFrame != "https://example.com/b.png" {
			t.Fatalf("frames = %q / %q", wire.firstFrame, wire.lastFrame)
		}
	})
}

func TestCompileContextIRPromptLength(t *testing.T) {
	request := contextIRRequest(
		[]message.Part{message.TextPart{Text: strings.Repeat("a", 7001)}},
		ContextIROptions{},
	)
	_, report, err := compileContextIRWire(t, "MiniMax-H3-Context-IR", request)
	if err == nil {
		t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
	}
	reason := rejectedReason(report, inference.FieldGenerateInputText)
	if !strings.Contains(reason, "at most 7000 characters") {
		t.Errorf("rejection reason = %q", reason)
	}

	request = contextIRRequest(
		[]message.Part{message.TextPart{Text: strings.Repeat("a", 7000)}},
		ContextIROptions{},
	)
	if _, report, err := compileContextIRWire(t, "MiniMax-H3-Context-IR", request); err != nil {
		t.Fatalf("7000-char prompt rejected: %v; report = %+v", err, report)
	}
}

func TestCompileContextIRIntents(t *testing.T) {
	temperature := 0.7
	maxTokens := 100
	callback := "https://example.com/callback"
	parts := []message.Part{message.TextPart{Text: "a cinematic scene"}}

	cases := []struct {
		name    string
		request inference.GenerateRequest
		field   inference.FieldID
		reason  string
	}{
		{
			name: "video intent rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent = inference.Intent{Video: &inference.VideoIntent{}}
				return request
			}(),
			field:  inference.FieldGenerateIntentVideo,
			reason: "not a video",
		},
		{
			name: "image intent rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent = inference.Intent{Image: &inference.ImageIntent{}}
				return request
			}(),
			field:  inference.FieldGenerateIntentImage,
			reason: "not an image",
		},
		{
			name: "audio intent rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent = inference.Intent{Audio: &inference.AudioIntent{}}
				return request
			}(),
			field:  inference.FieldGenerateIntentAudio,
			reason: "not audio",
		},
		{
			name: "sampling control rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent.Text.Temperature = &temperature
				return request
			}(),
			field:  inference.FieldGenerateIntentTemperature,
			reason: "no sampling controls",
		},
		{
			name: "token cap rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent.Text.MaxOutputTokens = &maxTokens
				return request
			}(),
			field:  inference.FieldGenerateIntentTextMaxOutputTokens,
			reason: "no token cap",
		},
		{
			name: "response shaping rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Input.Content.Intent.Text.Response = &inference.ResponseFormat{
					Kind: inference.ResponseJSONObject,
				}
				return request
			}(),
			field:  inference.FieldGenerateIntentTextResponse,
			reason: "plain enhanced prompt",
		},
		{
			name: "video options extension rejected",
			request: func() inference.GenerateRequest {
				request := contextIRRequest(parts, ContextIROptions{})
				request.Extensions = append(request.Extensions,
					VideoOptions{CallbackURL: callback})
				return request
			}(),
			field:  videoOptionsField("callback_url"),
			reason: "does not consume video_options",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, report, err := compileContextIRWire(t, "MiniMax-H3-Context-IR", tc.request)
			if err == nil {
				t.Fatalf("compile unexpectedly succeeded; report = %+v", report)
			}
			reason := rejectedReason(report, tc.field)
			if reason == "" {
				t.Fatalf("field %q not rejected; report = %+v", tc.field, report)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection reason = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestTransportContextIR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v2/h3_context_ir":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if body["model"] != "MiniMax-H3" ||
				body["duration"] != float64(5) ||
				body["ratio"] != "21:9" ||
				body["callback_url"] != "https://example.com/callback" {
				t.Errorf("create body = %#v", body)
			}
			content, ok := body["content"].([]any)
			if !ok || len(content) != 2 {
				t.Fatalf("content = %#v", body["content"])
			}
			text := content[0].(map[string]any)
			image := content[1].(map[string]any)
			if text["type"] != "text" || text["text"] != "a scene" ||
				image["type"] != "image_url" || image["role"] != "first_frame" {
				t.Errorf("content items = %#v / %#v", text, image)
			}
			_, _ = io.WriteString(writer, `{"task_id":"t-ir"}`)
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v2/query/video_generation/t-ir":
			_, _ = io.WriteString(writer, `{
				"task": {
					"id": "t-ir",
					"status": "succeeded",
					"content": {"prompt": "integrated_multimodal_description: [Shot 1] wide shot"},
					"usage": {"total_tokens": 100, "prompt_tokens": 40, "completion_tokens": 60}
				}
			}`)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	client := newMediaClient("sk-test", server.URL, Spec{})
	raw, err := transportContextIR(client, time.Millisecond)(context.Background(), contextIRWire{
		model:    "MiniMax-H3",
		prompt:   "a scene",
		duration: 5,
		ratio:    "21:9",
		v2Content: v2Content{
			firstFrame: "https://example.com/a.png",
		},
		callbackURL: "https://example.com/callback",
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if !strings.Contains(raw.prompt, "[Shot 1]") ||
		raw.inputTokens != 40 || raw.outputTokens != 60 ||
		raw.totalTokens != 100 || raw.requestID != "t-ir" {
		t.Fatalf("raw = %+v", raw)
	}
}

func TestTransportContextIRRequiresPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodPost:
			_, _ = io.WriteString(writer, `{"task_id":"t-ir"}`)
		case http.MethodGet:
			_, _ = io.WriteString(writer,
				`{"task":{"id":"t-ir","status":"succeeded","content":{}}}`)
		}
	}))
	defer server.Close()

	client := newMediaClient("sk-test", server.URL, Spec{})
	_, err := transportContextIR(client, time.Millisecond)(context.Background(), contextIRWire{
		model:    "MiniMax-H3",
		prompt:   "a scene",
		duration: 6,
	})
	if err == nil || !strings.Contains(err.Error(), "carries no prompt") {
		t.Fatalf("err = %v, want missing-prompt error", err)
	}
}
