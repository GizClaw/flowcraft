package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func boolPointer(value bool) *bool        { return &value }
func floatPointer(value float64) *float64 { return &value }
func intPointer(value int) *int           { return &value }

func TestUnaryTextOnWire(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("ok"))
	})

	runtime := newTestRuntime(t, server)
	response, err := runtime.Generate(context.Background(), kimiModel("moonshot-v1-8k"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if path := server.path(t, 0); path != "/chat/completions" {
		t.Fatalf("path = %q", path)
	}
	if server.streaming(t, 0) {
		t.Fatal("unary request must not stream")
	}
	body := server.body(t, 0)
	if body["model"] != "moonshot-v1-8k" {
		t.Fatalf("model = %v", body["model"])
	}
	messages := body["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("messages = %v", messages)
	}
	if stream, exists := body["stream"]; exists && stream != false {
		t.Fatalf("stream = %v", stream)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	text, ok := response.Message.Content.Parts[0].(message.TextPart)
	if !ok || text.Text != "ok" {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
	if response.Usage.InputTokens != 19 || response.Usage.OutputTokens != 21 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.Input.CacheReadTokens == nil || *response.Usage.Input.CacheReadTokens != 10 {
		t.Fatalf("cached tokens = %+v", response.Usage.Input.CacheReadTokens)
	}
	if response.Metadata.ResponseID != "cmpl-1" {
		t.Fatalf("response id = %q, want cmpl-1", response.Metadata.ResponseID)
	}
}

func TestMultimodalContentParts(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	videoData, err := media.NewVideoBytes([]byte("clip"), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("ok"))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("describe these")
	request.Input.Content.Parts = []message.Part{
		message.ImagePart{Source: image},
		message.VideoPart{Source: videoData},
		message.TextPart{Text: "describe these"},
	}
	if _, err := runtime.Generate(context.Background(), kimiModel("kimi-k3"), request); err != nil {
		t.Fatalf("generate: %v", err)
	}

	content := server.body(t, 0)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["type"] != "image_url" ||
		content[0].(map[string]any)["image_url"].(map[string]any)["url"] != "https://example.com/cat.png" {
		t.Fatalf("image part = %v", content[0])
	}
	video := content[1].(map[string]any)
	if video["type"] != "video_url" {
		t.Fatalf("video part = %v", video)
	}
	url := video["video_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:video/mp4;base64,") {
		t.Fatalf("inline video must render as data URI, got %q", url)
	}
	if content[2].(map[string]any)["text"] != "describe these" {
		t.Fatalf("text part = %v", content[2])
	}
}

func TestSamplingOnWire(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("{}"))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		MaxOutputTokens: intPointer(128),
		Temperature:     floatPointer(0.7),
		TopP:            floatPointer(0.9),
		Response:        &inference.ResponseFormat{Kind: inference.ResponseJSONObject},
	}
	if _, err := runtime.Generate(context.Background(), kimiModel("moonshot-v1-32k"), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	body := server.body(t, 0)
	if body["max_completion_tokens"] != float64(128) {
		t.Fatalf("max_completion_tokens = %v", body["max_completion_tokens"])
	}
	if body["temperature"] != 0.7 || body["top_p"] != 0.9 {
		t.Fatalf("sampling = %v %v", body["temperature"], body["top_p"])
	}
	if body["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
}

func TestJSONSchemaOnWire(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("{}"))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		Response: &inference.ResponseFormat{
			Kind:   inference.ResponseJSONSchema,
			Name:   "answer",
			Schema: []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		},
	}
	if _, err := runtime.Generate(context.Background(), kimiModel("kimi-k2.5"), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	format := server.body(t, 0)["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format = %v", format)
	}
	schema := format["json_schema"].(map[string]any)
	if schema["name"] != "answer" || schema["strict"] != true {
		t.Fatalf("json_schema = %v", schema)
	}
}

func TestSamplingDropsOnKModels(t *testing.T) {
	compiled, err := compileGenerate("kimi-k2.6", catalog["kimi-k2.6"])(
		context.Background(),
		kimiModel("kimi-k2.6"),
		func() inference.GenerateRequest {
			request := simpleTextRequest("hi")
			request.Input.Content.Intent.Text = &inference.TextIntent{
				Temperature: floatPointer(0.7),
			}
			return request
		}(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Temperature != nil {
		t.Fatalf("temperature must drop on K models, got %v", *compiled.Wire.Temperature)
	}
	found := false
	for _, decision := range compiled.Report.Decisions {
		if decision.Field == inference.FieldGenerateIntentTemperature {
			found = true
			if decision.Disposition != inference.Dropped {
				t.Fatalf("temperature decision = %v", decision.Disposition)
			}
		}
	}
	if !found {
		t.Fatal("temperature decision missing from report")
	}
}

func TestToolsOnWire(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, completionBody(map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"北京"}`,
				},
			}},
		}, "tool_calls"))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("北京今天天气怎么样？")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		Tools: []message.Definition{{
			Name:        "get_weather",
			Description: "获取指定城市的天气",
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
		ToolChoice: &inference.ToolChoice{Kind: inference.ToolChoiceRequired},
	}
	response, err := runtime.Generate(context.Background(), kimiModel("kimi-k2.6"), request)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	body := server.body(t, 0)
	tools := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tools = %v", tools)
	}
	if body["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	call, ok := response.Message.Content.Parts[0].(message.ToolCallPart)
	if !ok || call.Call.Name != "get_weather" || call.Call.ID != "call_1" {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}
}

func TestNamedToolChoiceOnWire(t *testing.T) {
	compiled, err := compileGenerate("kimi-k3", catalog["kimi-k3"])(
		context.Background(),
		kimiModel("kimi-k3"),
		func() inference.GenerateRequest {
			request := simpleTextRequest("hi")
			request.Input.Content.Intent.Text = &inference.TextIntent{
				Tools: []message.Definition{{
					Name:        "get_weather",
					InputSchema: []byte(`{"type":"object"}`),
				}},
				ToolChoice: &inference.ToolChoice{Kind: inference.ToolChoiceNamed, Name: "get_weather"},
			}
			return request
		}(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var body map[string]any
	raw, err := compiled.Wire.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", choice)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("晴，25°C"))
	})

	runtime := newTestRuntime(t, server)
	request := inference.GenerateRequest{
		Context: []message.Message{
			{
				Role: message.RoleUser,
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "北京天气？"},
				}},
			},
			{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.ToolCallPart{Call: message.Call{
						ID:        "call_1",
						Name:      "get_weather",
						Arguments: []byte(`{"city":"北京"}`),
					}},
				}},
			},
			{
				Role: message.RoleTool,
				Content: message.Content{Parts: []message.Part{
					message.ToolResultPart{Result: message.Result{CallID: "call_1", Content: "晴，25°C"}},
				}},
			},
		},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "谢谢"}}},
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
	if _, err := runtime.Generate(context.Background(), kimiModel("moonshot-v1-8k"), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	messages := server.body(t, 0)["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %v", messages)
	}
	assistant := messages[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("assistant tool calls = %v", calls)
	}
	toolMessage := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_1" || toolMessage["content"] != "晴，25°C" {
		t.Fatalf("tool message = %v", toolMessage)
	}
}

func TestReasoningDialects(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		enabled      *bool
		effort       inference.ReasoningEffort
		wantEffort   string
		wantThinking string // thinking.type on the wire; "" = absent
		wantReject   inference.FieldID
	}{
		{name: "k3 effort low", model: "kimi-k3", effort: inference.ReasoningLow, wantEffort: "low"},
		{name: "k3 effort medium quantizes", model: "kimi-k3", effort: inference.ReasoningMedium, wantEffort: "high"},
		{name: "k3 effort high", model: "kimi-k3", effort: inference.ReasoningHigh, wantEffort: "high"},
		{name: "k3 disable rejects", model: "kimi-k3", enabled: boolPointer(false), wantReject: inference.FieldGenerateIntentReasoningEnabled},
		{name: "k3 explicit enable no-op", model: "kimi-k3", enabled: boolPointer(true)},
		{name: "k2.7 disable rejects", model: "kimi-k2.7-code", enabled: boolPointer(false), wantReject: inference.FieldGenerateIntentReasoningEnabled},
		{name: "k2.6 enable", model: "kimi-k2.6", enabled: boolPointer(true), wantThinking: "enabled"},
		{name: "k2.6 disable", model: "kimi-k2.6", enabled: boolPointer(false), wantThinking: "disabled"},
		{name: "k2.6 effort drops", model: "kimi-k2.6", effort: inference.ReasoningHigh},
		{name: "moonshot rejects reasoning", model: "moonshot-v1-8k", enabled: boolPointer(true), wantReject: inference.FieldGenerateIntentReasoningEnabled},
		{name: "moonshot rejects effort", model: "moonshot-v1-8k", effort: inference.ReasoningLow, wantReject: inference.FieldGenerateIntentReasoningEffort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := simpleTextRequest("hi")
			request.Input.Content.Intent.Text = &inference.TextIntent{
				ReasoningEnabled: tc.enabled,
				ReasoningEffort:  tc.effort,
			}
			compiled, err := compileGenerate(tc.model, catalog[tc.model])(
				context.Background(),
				kimiModel(tc.model),
				request,
				inference.GenerateExecutionUnary,
			)
			if tc.wantReject != "" {
				if !inference.IsKind(err, inference.UnsupportedFeature) {
					t.Fatalf("error = %v, want unsupported_feature", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if compiled.Wire.Effort != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", compiled.Wire.Effort, tc.wantEffort)
			}
			thinking := ""
			if compiled.Wire.Thinking != nil {
				thinking = compiled.Wire.Thinking.Type
			}
			if thinking != tc.wantThinking {
				t.Fatalf("thinking = %q, want %q", thinking, tc.wantThinking)
			}
		})
	}
}

func TestReasoningRoundTrip(t *testing.T) {
	// Unary response carries reasoning_content; feeding it back must
	// round-trip as reasoning_content plus thinking.keep="all" on k2.6.
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, completionBody(map[string]any{
			"role":              "assistant",
			"content":           "4",
			"reasoning_content": "2+2",
		}, "stop"))
	})

	runtime := newTestRuntime(t, server)
	response, err := runtime.Generate(context.Background(), kimiModel("kimi-k2.6"), simpleTextRequest("2+2=?"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	reasoning, ok := response.Message.Content.Parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "2+2" {
		t.Fatalf("parts = %#v", response.Message.Content.Parts)
	}

	followUp := inference.GenerateRequest{
		Context: []message.Message{
			{Role: message.RoleUser, Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "2+2=?"},
			}}},
			response.Message,
		},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "and 3+3?"}}},
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
	if _, err := runtime.Generate(context.Background(), kimiModel("kimi-k2.6"), followUp); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	body := server.body(t, 1)
	assistant := body["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "2+2" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["keep"] != "all" {
		t.Fatalf("thinking.keep must default to all when history carries a trace: %v", thinking)
	}
}

func TestReasoningHistoryDropsOnK25(t *testing.T) {
	compiled, err := compileGenerate("kimi-k2.5", catalog["kimi-k2.5"])(
		context.Background(),
		kimiModel("kimi-k2.5"),
		inference.GenerateRequest{
			Context: []message.Message{
				{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
					message.ReasoningPart{Text: "trace"},
					message.TextPart{Text: "4"},
				}}},
			},
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
					Intent:  inference.Intent{Text: &inference.TextIntent{}},
				},
			},
		},
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Messages[0].Reasoning != "" {
		t.Fatalf("reasoning must drop on k2.5, got %q", compiled.Wire.Messages[0].Reasoning)
	}
}

func TestPreserveThinkingOverride(t *testing.T) {
	request := inference.GenerateRequest{
		Context: []message.Message{
			{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
				message.ReasoningPart{Text: "trace"},
				message.TextPart{Text: "4"},
			}}},
		},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
		Extensions: inference.Extensions{GenerateOptions{PreserveThinking: boolPointer(false)}},
	}
	compiled, err := compileGenerate("kimi-k2.6", catalog["kimi-k2.6"])(
		context.Background(),
		kimiModel("kimi-k2.6"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.Thinking != nil && compiled.Wire.Thinking.Keep != "" {
		t.Fatalf("override must force keep off: %+v", compiled.Wire.Thinking)
	}
}

func TestPromptCacheKeyOnWire(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textCompletion("ok"))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{GenerateOptions{PromptCacheKey: "session-42"}}
	if _, err := runtime.Generate(context.Background(), kimiModel("kimi-k3"), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if key := server.body(t, 0)["prompt_cache_key"]; key != "session-42" {
		t.Fatalf("prompt_cache_key = %v", key)
	}
}

func TestRejectsForeignExtension(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{foreignExtension{}}
	_, err := compileGenerate("kimi-k3", catalog["kimi-k3"])(
		context.Background(),
		kimiModel("kimi-k3"),
		request,
		inference.GenerateExecutionUnary,
	)
	if !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("error = %v, want invalid_extension", err)
	}
}

type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return "kimi" }
func (foreignExtension) ExtensionID() string { return "foreign" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"x"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

func TestCompileRejections(t *testing.T) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	video, err := media.NewVideoURL("https://example.com/clip.mp4", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		model  string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
	}{
		{
			name:  "image on text model",
			model: "moonshot-v1-8k",
			mutate: func(request *inference.GenerateRequest) {
				request.Input.Content.Parts = []message.Part{message.ImagePart{Source: image}}
			},
			field: inference.FieldGenerateInputImage,
		},
		{
			name:  "video on vision-only model",
			model: "kimi-k2.6",
			mutate: func(request *inference.GenerateRequest) {
				request.Input.Content.Parts = []message.Part{message.VideoPart{Source: video}}
			},
			field: inference.FieldGenerateInputVideo,
		},
		{
			name:  "audio intent",
			model: "kimi-k3",
			mutate: func(request *inference.GenerateRequest) {
				request.Input.Content.Intent.Audio = &inference.AudioIntent{
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}
			},
			field: inference.FieldGenerateIntentAudio,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := simpleTextRequest("hi")
			tc.mutate(&request)
			_, err := compileGenerate(tc.model, catalog[tc.model])(
				context.Background(),
				kimiModel(tc.model),
				request,
				inference.GenerateExecutionUnary,
			)
			if !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("error = %v, want unsupported_feature on %s", err, tc.field)
			}
		})
	}
}

func TestStreamTextAndUsage(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, chunkBody(
			streamChunk(map[string]any{"role": "assistant", "content": ""}, nil, false),
			streamChunk(map[string]any{"content": "你"}, nil, false),
			streamChunk(map[string]any{"content": "好"}, nil, false),
			streamChunk(map[string]any{}, "stop", true),
		))
	})

	runtime := newTestRuntime(t, server)
	stream, err := runtime.GenerateStream(context.Background(), kimiModel("kimi-k2.6"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if !server.streaming(t, 0) {
		t.Fatal("stream request must ask for SSE")
	}
	if streamFlag := server.body(t, 0)["stream"]; streamFlag != true {
		t.Fatalf("stream = %v", streamFlag)
	}

	var text string
	var usage *inference.Usage
	var finish inference.FinishReason
	var responseID string
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch delta := event.Delta.(type) {
		case inference.TextPartDelta:
			text += delta.Text
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
			responseID = event.ResponseID
		}
	}
	if text != "你好" || finish != inference.FinishCompleted {
		t.Fatalf("text = %q finish = %q", text, finish)
	}
	if usage == nil || usage.InputTokens != 19 || usage.OutputTokens != 13 {
		t.Fatalf("usage = %+v", usage)
	}
	if responseID != "cmpl-1" {
		t.Fatalf("stream response id = %q, want cmpl-1", responseID)
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Metadata.ResponseID != "cmpl-1" {
		t.Fatalf("result response id = %q, want cmpl-1", result.Metadata.ResponseID)
	}
}

func TestStreamReasoningAndToolCalls(t *testing.T) {
	server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, chunkBody(
			streamChunk(map[string]any{"role": "assistant", "reasoning_content": "想"}, nil, false),
			streamChunk(map[string]any{"reasoning_content": "一下"}, nil, false),
			streamChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"ci`},
			}}}, nil, false),
			streamChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": `ty":"北京"}`},
			}}}, nil, false),
			streamChunk(map[string]any{}, "tool_calls", true),
		))
	})

	runtime := newTestRuntime(t, server)
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		Tools: []message.Definition{{
			Name:        "get_weather",
			Description: "获取指定城市的天气",
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	stream, err := runtime.GenerateStream(context.Background(), kimiModel("kimi-k2.6"), request)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var reasoning string
	var toolID, toolName, toolArgs string
	var finish inference.FinishReason
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch delta := event.Delta.(type) {
		case inference.ReasoningDelta:
			reasoning += delta.Text
		case inference.ToolCallDelta:
			if delta.ID != "" {
				toolID = delta.ID
			}
			if delta.Name != "" {
				toolName = delta.Name
			}
			toolArgs += delta.ArgumentsFragment
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
	}
	if reasoning != "想一下" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if toolID != "call_1" || toolName != "get_weather" || toolArgs != `{"city":"北京"}` {
		t.Fatalf("tool call = %q %q %q", toolID, toolName, toolArgs)
	}
	if finish != inference.FinishToolCalls {
		t.Fatalf("finish = %q", finish)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		check     func(error) bool
		requestID string
	}{
		{name: "rate limit", status: 429, body: `{"error":{"type":"rate_limit_error","message":"slow down"},"request_id":"kr-1"}`, check: errdefs.IsRateLimit, requestID: "kr-1"},
		{name: "bad key", status: 401, body: `{"error":{"type":"authentication_error","message":"bad key"}}`, check: errdefs.IsUnauthorized},
		{name: "invalid request", status: 400, body: `{"error":{"type":"invalid_request_error","message":"bad model"}}`, check: errdefs.IsValidation},
		{name: "server error", status: 500, body: `{"error":{"type":"server_error","message":"boom"}}`, check: errdefs.IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newKimiServer(t, func(w http.ResponseWriter, _ map[string]any) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			runtime := newTestRuntime(t, server)
			_, err := runtime.Generate(context.Background(), kimiModel("moonshot-v1-8k"), simpleTextRequest("hi"))
			if !tc.check(err) {
				t.Fatalf("error = %v", err)
			}
			if tc.requestID != "" {
				if got, ok := errdefs.RequestID(err); !ok || got != tc.requestID {
					t.Fatalf("RequestID = %q/%v, want %q/true", got, ok, tc.requestID)
				}
			}
		})
	}
}
