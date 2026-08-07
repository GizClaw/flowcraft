package qwen

import (
	"context"
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

// Generate pipeline behavior tests, run end to end through the runtime
// against the fake DashScope endpoint: the native envelope, endpoint
// selection, the thinking dialect, the reasoning round-trip, capability
// gating, streaming, and error classification.

// TestUnaryTextOnWire asserts the native request shape: model,
// input.messages, parameters.result_format=message, on the
// text-generation path.
func TestUnaryTextOnWire(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textEnvelope("ok"))
	})
	runtime := newTestRuntime(t, server)

	response, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	if text, ok := parts[0].(message.TextPart); !ok || text.Text != "ok" {
		t.Fatalf("part = %#v", parts[0])
	}
	if response.Usage.TotalTokens != 19 {
		t.Fatalf("usage = %+v", response.Usage)
	}

	if path := server.path(t, 0); path != pathTextGeneration {
		t.Fatalf("path = %q", path)
	}
	body := server.body(t, 0)
	if body["model"] != "qwen-plus" {
		t.Fatalf("model = %v", body["model"])
	}
	input := body["input"].(map[string]any)
	messages := input["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v", messages)
	}
	message := messages[0].(map[string]any)
	if message["role"] != "user" || message["content"] != "hi" {
		t.Fatalf("message = %v", message)
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["result_format"] != "message" {
		t.Fatalf("result_format = %v", parameters["result_format"])
	}
	if _, exists := parameters["incremental_output"]; exists {
		t.Fatalf("unary must not set incremental_output: %v", parameters)
	}
}

// TestMultimodalEndpointSelection asserts vision models ride the
// multimodal-generation path with array-shaped message content.
func TestMultimodalEndpointSelection(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		// Multimodal models answer content as an array of text items.
		_, _ = fmt.Fprint(w, dashEnvelope(map[string]any{
			"role": "assistant",
			"content": []any{map[string]any{
				"text": "a dog and a girl",
			}},
		}, "stop"))
	})
	runtime := newTestRuntime(t, server)

	image, err := media.NewImageURL("https://example.com/dog.jpg", "")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.ImagePart{Source: image},
					message.TextPart{Text: "what is this?"},
				}},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
	response, err := runtime.Generate(context.Background(), qwenModel("qwen3-vl-plus"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	text, ok := response.Message.Content.Parts[0].(message.TextPart)
	if !ok || text.Text != "a dog and a girl" {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}

	if path := server.path(t, 0); path != pathMultimodalGeneration {
		t.Fatalf("path = %q", path)
	}
	body := server.body(t, 0)
	messages := body["input"].(map[string]any)["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["image"] != "https://example.com/dog.jpg" {
		t.Fatalf("image item = %v", content[0])
	}
	if content[1].(map[string]any)["text"] != "what is this?" {
		t.Fatalf("text item = %v", content[1])
	}
}

// TestSamplingOnWire asserts the flattened text intent compiles onto
// parameters: max_tokens (answer-only, the canonical semantic),
// temperature, top_p, and the json_object response format.
func TestSamplingOnWire(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textEnvelope("{}"))
	})
	runtime := newTestRuntime(t, server)

	maxTokens := 64
	temperature := 0.2
	topP := 0.5
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.MaxOutputTokens = &maxTokens
	request.Input.Content.Intent.Text.Temperature = &temperature
	request.Input.Content.Intent.Text.TopP = &topP
	request.Input.Content.Intent.Text.Response = &inference.ResponseFormat{
		Kind: inference.ResponseJSONObject,
	}
	if _, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parameters := server.body(t, 0)["parameters"].(map[string]any)
	if parameters["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v", parameters["max_tokens"])
	}
	if parameters["temperature"] != 0.2 || parameters["top_p"] != 0.5 {
		t.Fatalf("sampling = %v", parameters)
	}
	if _, exists := parameters["max_completion_tokens"]; exists {
		t.Fatalf("must not set max_completion_tokens: %v", parameters)
	}
	format := parameters["response_format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("response_format = %v", format)
	}
}

// TestToolsOnWire asserts tool definitions compile to parameters.tools
// with the named tool choice, and that a tool_calls answer decodes into a
// ToolCallPart.
func TestToolsOnWire(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, dashEnvelope(map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"hz"}`,
				},
			}},
		}, "tool_calls"))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("weather?")
	request.Input.Content.Intent.Text.Tools = []message.Definition{{
		Name:        "get_weather",
		Description: "weather lookup",
		InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	request.Input.Content.Intent.Text.ToolChoice = &inference.ToolChoice{
		Kind: inference.ToolChoiceNamed,
		Name: "get_weather",
	}
	response, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %#v", parts)
	}
	call, ok := parts[0].(message.ToolCallPart)
	if !ok {
		t.Fatalf("part = %#v", parts[0])
	}
	if call.Call.ID != "call_1" || call.Call.Name != "get_weather" {
		t.Fatalf("call = %#v", call.Call)
	}
	if string(call.Call.Arguments) != `{"city":"hz"}` {
		t.Fatalf("arguments = %s", call.Call.Arguments)
	}

	parameters := server.body(t, 0)["parameters"].(map[string]any)
	tools := parameters["tools"].([]any)
	definition := tools[0].(map[string]any)["function"].(map[string]any)
	if definition["name"] != "get_weather" {
		t.Fatalf("tools = %v", tools)
	}
	choice := parameters["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", choice)
	}
}

// TestToolResultRoundTrip asserts tool results compile to role=tool
// messages carrying the call id.
func TestToolResultRoundTrip(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textEnvelope("sunny"))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("and tomorrow?")
	request.Context = []message.Message{
		{Role: message.RoleUser, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "weather?"},
		}}},
		{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: message.Call{
				ID:        "call_1",
				Name:      "get_weather",
				Arguments: []byte(`{}`),
			}},
		}}},
		{Role: message.RoleTool, Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.Result{
				CallID:  "call_1",
				Content: "sunny",
			}},
		}}},
	}
	if _, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	messages := server.body(t, 0)["input"].(map[string]any)["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %v", messages)
	}
	assistant := messages[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("assistant = %v", assistant)
	}
	toolMessage := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", toolMessage)
	}
}

// TestThinkingStreamOnly asserts thinking mode rejects unary compiles on
// the commercial thinking models — the protocol answers thinking requests
// on SSE only — and compiles cleanly on a stream.
func TestThinkingStreamOnly(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(
			streamChunk(map[string]any{
				"role":              "assistant",
				"reasoning_content": "thinking",
			}, nil, false),
			streamChunk(map[string]any{
				"role":    "assistant",
				"content": "answer",
			}, "stop", true),
		))
	})
	runtime := newTestRuntime(t, server)

	enabled := true
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEnabled = &enabled

	_, err := runtime.Generate(context.Background(), qwenModel("qwen3.7-plus"), request)
	if err == nil || !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("unary thinking err = %v", err)
	}

	stream, err := runtime.GenerateStream(context.Background(), qwenModel("qwen3.7-plus"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	parts := result.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %#v", parts)
	}
	if reasoning, ok := parts[0].(message.ReasoningPart); !ok || reasoning.Text != "thinking" {
		t.Fatalf("reasoning = %#v", parts[0])
	}
	if text, ok := parts[1].(message.TextPart); !ok || text.Text != "answer" {
		t.Fatalf("text = %#v", parts[1])
	}

	body := server.body(t, 0)
	if !server.streaming(t, 0) {
		t.Fatal("stream request must carry the SSE header")
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["enable_thinking"] != true || parameters["incremental_output"] != true {
		t.Fatalf("parameters = %v", parameters)
	}
}

// TestReasoningEffortDialect asserts the effort dialect: levels exist only
// on qwen3.8-max-preview (canonical high maps to DashScope's top level,
// xhigh); on other thinking models an explicit effort drops with a reason.
func TestReasoningEffortDialect(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(streamChunk(map[string]any{
			"role":    "assistant",
			"content": "ok",
		}, "stop", true)))
	})
	runtime := newTestRuntime(t, server)

	enabled := true
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.ReasoningEnabled = &enabled
	request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningHigh

	stream, err := runtime.GenerateStream(context.Background(), qwenModel("qwen3.8-max-preview"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		if _, err = stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	parameters := server.body(t, 0)["parameters"].(map[string]any)
	if parameters["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v", parameters["reasoning_effort"])
	}

	// qwen3.7-plus has no effort levels: the effort drops with a reason
	// rather than silently compiling to a wrong parameter.
	stream, err = runtime.GenerateStream(context.Background(), qwenModel("qwen3.7-plus"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		if _, err = stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	parameters = server.body(t, 1)["parameters"].(map[string]any)
	if _, exists := parameters["reasoning_effort"]; exists {
		t.Fatalf("effort must drop on qwen3.7-plus: %v", parameters)
	}
}

// TestReasoningRoundTrip asserts assistant reasoning history compiles to
// reasoning_content with preserve_thinking defaulted on, and drops with a
// reason on models that cannot re-ingest traces.
func TestReasoningRoundTrip(t *testing.T) {
	request := func() inference.GenerateRequest {
		r := simpleTextRequest("next")
		r.Context = []message.Message{{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ReasoningPart{Text: "earlier thinking"},
			message.TextPart{Text: "earlier answer"},
		}}}}
		return r
	}

	streamServer := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(streamChunk(map[string]any{
			"role":    "assistant",
			"content": "ok",
		}, "stop", true)))
	})
	streamRuntime := newTestRuntime(t, streamServer)

	stream, err := streamRuntime.GenerateStream(context.Background(), qwenModel("qwen3.7-plus"), request())
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		if _, err = stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	body := streamServer.body(t, 0)
	messages := body["input"].(map[string]any)["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if assistant["reasoning_content"] != "earlier thinking" {
		t.Fatalf("assistant = %v", assistant)
	}
	if body["parameters"].(map[string]any)["preserve_thinking"] != true {
		t.Fatalf("preserve_thinking must default on: %v", body["parameters"])
	}

	// qwen-plus cannot re-ingest traces: the reasoning part drops.
	unaryServer := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, textEnvelope("ok"))
	})
	unaryRuntime := newTestRuntime(t, unaryServer)
	if _, err := unaryRuntime.Generate(context.Background(), qwenModel("qwen-plus"), request()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	messages = unaryServer.body(t, 0)["input"].(map[string]any)["messages"].([]any)
	assistant = messages[0].(map[string]any)
	if _, exists := assistant["reasoning_content"]; exists {
		t.Fatalf("trace must drop on qwen-plus: %v", assistant)
	}
	if assistant["content"] != "earlier answer" {
		t.Fatalf("assistant = %v", assistant)
	}
}

// TestGenerateOptionsExtension asserts extension fields compile onto
// parameters and that thinking controls on a non-thinking model reject as
// InvalidExtension.
func TestGenerateOptionsExtension(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(streamChunk(map[string]any{
			"role":    "assistant",
			"content": "ok",
		}, "stop", true)))
	})
	runtime := newTestRuntime(t, server)

	topK := int64(20)
	budget := int64(4096)
	presence := 1.5
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{GenerateOptions{
		ThinkingBudget:  &budget,
		TopK:            &topK,
		PresencePenalty: &presence,
	}}

	stream, err := runtime.GenerateStream(context.Background(), qwenModel("qwen3.7-plus"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		if _, err = stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	parameters := server.body(t, 0)["parameters"].(map[string]any)
	if parameters["thinking_budget"] != float64(4096) {
		t.Fatalf("thinking_budget = %v", parameters["thinking_budget"])
	}
	if parameters["top_k"] != float64(20) || parameters["presence_penalty"] != 1.5 {
		t.Fatalf("parameters = %v", parameters)
	}

	// thinking_budget on a model without a thinking mode rejects.
	_, err = runtime.Generate(context.Background(), qwenModel("qwen-plus"), request)
	if err == nil || !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("err = %v", err)
	}
}

// foreignExtension stands in for an extension another operation family
// owns; attaching it to generate rejects as InvalidExtension.
type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return "qwen" }
func (foreignExtension) ExtensionID() string { return "music_options" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"lyrics"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

func TestRejectsForeignExtension(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("rejected request must not reach transport")
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{foreignExtension{}}
	_, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), request)
	if err == nil || !inference.IsKind(err, inference.InvalidExtension) {
		t.Fatalf("err = %v", err)
	}
}

// TestCompileRejections table-drives the capability gates.
func TestCompileRejections(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		t.Error("rejected request must not reach transport")
	})
	runtime := newTestRuntime(t, server)

	image, err := media.NewImageURL("https://example.com/a.jpg", "")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	video, err := media.NewVideoURL("https://example.com/a.mp4", "")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	enabled := true

	cases := []struct {
		name   string
		model  string
		mutate func(*inference.GenerateRequest)
	}{
		{
			name:  "image on text model",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Parts = append(r.Input.Content.Parts, message.ImagePart{Source: image})
			},
		},
		{
			name:  "video on vision-only model",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Parts = append(r.Input.Content.Parts, message.VideoPart{Source: video})
			},
		},
		{
			name:  "audio intent",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio = &inference.AudioIntent{
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}
			},
		},
		{
			name:  "image intent",
			model: "qwen3-vl-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image = &inference.ImageIntent{}
			},
		},
		{
			name:  "required tool choice",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Text.Tools = []message.Definition{{
					Name:        "get_weather",
					Description: "weather lookup",
					InputSchema: []byte(`{"type":"object"}`),
				}}
				r.Input.Content.Intent.Text.ToolChoice = &inference.ToolChoice{Kind: inference.ToolChoiceRequired}
			},
		},
		{
			name:  "json schema response",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Text.Response = &inference.ResponseFormat{
					Kind:   inference.ResponseJSONSchema,
					Name:   "answer",
					Schema: []byte(`{"type":"object"}`),
				}
			},
		},
		{
			name:  "reasoning on non-thinking model",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Text.ReasoningEnabled = &enabled
			},
		},
		{
			name:  "effort on non-thinking model",
			model: "qwen-plus",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningLow
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := simpleTextRequest("hi")
			tc.mutate(&request)
			_, err := runtime.Generate(context.Background(), qwenModel(tc.model), request)
			if err == nil || !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestStreamToolCalls asserts streamed tool-call deltas accumulate into
// one ToolCallPart with the arguments fragments concatenated.
func TestStreamToolCalls(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(
			streamChunk(map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":    "call_1",
					"type":  "function",
					"index": 0,
					"function": map[string]any{
						"name":      "get_weather",
						"arguments": `{"city":`,
					},
				}},
			}, nil, false),
			streamChunk(map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"type":  "function",
					"index": 0,
					"function": map[string]any{
						"arguments": `"hz"}`,
					},
				}},
			}, nil, false),
			streamChunk(map[string]any{
				"role":    "assistant",
				"content": "",
			}, "tool_calls", true),
		))
	})
	runtime := newTestRuntime(t, server)

	request := simpleTextRequest("weather?")
	request.Input.Content.Intent.Text.Tools = []message.Definition{{
		Name:        "get_weather",
		Description: "weather lookup",
		InputSchema: []byte(`{"type":"object"}`),
	}}
	stream, err := runtime.GenerateStream(context.Background(), qwenModel("qwen-plus"), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var events int
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events++
	}
	if events != 3 {
		t.Fatalf("events = %d", events)
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", result.FinishReason)
	}
	parts := result.Message.Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %#v", parts)
	}
	call, ok := parts[0].(message.ToolCallPart)
	if !ok {
		t.Fatalf("part = %#v", parts[0])
	}
	if call.Call.ID != "call_1" || call.Call.Name != "get_weather" {
		t.Fatalf("call = %#v", call.Call)
	}
	if string(call.Call.Arguments) != `{"city":"hz"}` {
		t.Fatalf("arguments = %s", call.Call.Arguments)
	}
}

// TestErrorClassification asserts HTTP statuses and DashScope code strings
// classify into the errdefs taxonomy.
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{name: "rate limit", status: 429, body: `{"code":"Throttling.RateQuota","message":"slow down"}`, check: errdefs.IsRateLimit},
		{name: "bad api key", status: 401, body: `{"code":"InvalidApiKey","message":"bad key"}`, check: errdefs.IsUnauthorized},
		{name: "arrearage", status: 403, body: `{"code":"Arrearage","message":"overdue"}`, check: errdefs.IsForbidden},
		{name: "invalid param", status: 400, body: `{"code":"InvalidParameter","message":"bad model"}`, check: errdefs.IsValidation},
		{name: "server error", status: 500, body: `{"code":"InternalError","message":"boom"}`, check: errdefs.IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			runtime := newTestRuntime(t, server)
			_, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), simpleTextRequest("hi"))
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestEnvelopeErrorOn200 asserts a non-empty code on a 200 envelope
// classifies as a provider failure.
func TestEnvelopeErrorOn200(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, `{"status_code":200,"code":"Throttling.RateQuota","message":"slow down"}`)
	})
	runtime := newTestRuntime(t, server)
	_, err := runtime.Generate(context.Background(), qwenModel("qwen-plus"), simpleTextRequest("hi"))
	if err == nil || !errdefs.IsRateLimit(err) {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamEnvelopeError asserts a mid-stream error envelope surfaces
// instead of being swallowed.
func TestStreamEnvelopeError(t *testing.T) {
	server := newDashServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, dashSSEBody(
			streamChunk(map[string]any{
				"role":    "assistant",
				"content": "partial",
			}, nil, false),
			map[string]any{
				"status_code": 200,
				"code":        "Throttling.RateQuota",
				"message":     "slow down",
			},
		))
	})
	runtime := newTestRuntime(t, server)
	stream, err := runtime.GenerateStream(context.Background(), qwenModel("qwen-plus"), simpleTextRequest("hi"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			t.Fatal("stream must surface the error, not end cleanly")
		}
		if err != nil {
			if !strings.Contains(err.Error(), "Throttling") && !errdefs.IsRateLimit(err) {
				t.Fatalf("err = %v", err)
			}
			return
		}
	}
}
