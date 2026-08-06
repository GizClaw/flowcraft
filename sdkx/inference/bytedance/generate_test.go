package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// capturedArk serves the Responses API surface used by the generate drivers
// and records every request body for assertion.
type capturedArk struct {
	t *testing.T

	bodies  []map[string]any
	handler func(w http.ResponseWriter, body map[string]any, stream bool)
}

func newCapturedArk(
	t *testing.T,
	handler func(w http.ResponseWriter, body map[string]any, stream bool),
) (*httptest.Server, *capturedArk) {
	capture := &capturedArk{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var body map[string]any
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("body is not JSON: %v", err)
				return
			}
		}
		stream, _ := body["stream"].(bool)
		capture.bodies = append(capture.bodies, body)
		handler(w, body, stream)
	}))
	return server, capture
}

func (c *capturedArk) body(index int) map[string]any {
	c.t.Helper()
	if index >= len(c.bodies) {
		c.t.Fatalf("only %d captured requests", len(c.bodies))
	}
	return c.bodies[index]
}

func newTestRuntime(
	t *testing.T,
	server *httptest.Server,
) *inference.Runtime {
	t.Helper()
	provider := buildProvider(t, map[string]any{
		"base_url": server.URL + "/api/v3",
	}, speechProfiles())
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func generateModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "bytedance", Name: name},
		Profile: "default",
	}
}

func responsesResponseJSON(output []map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id":     "resp_1",
		"object": "response",
		"status": "completed",
		"output": output,
		"usage": map[string]any{
			"input_tokens":          12,
			"output_tokens":         7,
			"total_tokens":          19,
			"input_tokens_details":  map[string]any{"cached_tokens": 3},
			"output_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	})
	return string(payload)
}

func textOutputItem(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
}

func TestGenerateUnaryCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem(`{"answer":"ok"}`),
		}))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	maxTokens := 64
	temperature := 0.5
	topP := 0.9
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		inference.GenerateRequest{
			Context: []message.Message{
				{
					Role: message.RoleSystem,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "be terse"},
					}},
				},
				{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "prior answer"},
						message.ToolCallPart{Call: message.Call{
							ID:        "call_1",
							Name:      "lookup",
							Arguments: json.RawMessage(`{"q":"x"}`),
						}},
					}},
				},
				{
					Role: message.RoleTool,
					Content: message.Content{Parts: []message.Part{
						message.ToolResultPart{Result: message.Result{
							CallID:  "call_1",
							Content: "result text",
						}},
					}},
				},
			},
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{
						Parts: []message.Part{
							message.TextPart{Text: "current question"},
						},
					},
					Intent: inference.Intent{
						Text: &inference.TextIntent{
							Response:        &inference.ResponseFormat{Kind: inference.ResponseJSONObject},
							MaxOutputTokens: &maxTokens,
							Tools: []message.Definition{{
								Name:        "lookup",
								Description: "look things up",
								InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
							}},
							ToolChoice:      &inference.ToolChoice{Kind: inference.ToolChoiceAuto},
							Temperature:     &temperature,
							TopP:            &topP,
							ReasoningEffort: inference.ReasoningHigh,
						},
					},
				},
			},
		},
	)
	if err != nil {
		for depth, current := 0, err; current != nil && depth < 8; depth++ {
			t.Logf("depth %d: %T: %v", depth, current, current)
			current = errors.Unwrap(current)
		}
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	text, ok := response.Message.Content.Parts[0].(message.TextPart)
	if !ok || text.Text != `{"answer":"ok"}` {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if response.Usage.TotalTokens != 19 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.Input.CacheReadTokens == nil ||
		*response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cached = %+v", response.Usage.Input)
	}
	if response.Usage.Output.ReasoningTokens == nil ||
		*response.Usage.Output.ReasoningTokens != 2 {
		t.Fatalf("reasoning = %+v", response.Usage.Output)
	}

	body := capture.body(0)
	if body["model"] != "doubao-seed-2-1-pro" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["instructions"] != "be terse" {
		t.Fatalf("instructions = %v", body["instructions"])
	}
	if body["max_output_tokens"].(float64) != 64 {
		t.Fatalf("max_output_tokens = %v", body["max_output_tokens"])
	}
	if body["temperature"].(float64) != 0.5 || body["top_p"].(float64) != 0.9 {
		t.Fatalf("sampling = %v / %v", body["temperature"], body["top_p"])
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v", body["thinking"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %v", body["reasoning"])
	}
	text_, _ := body["text"].(map[string]any)
	format, _ := text_["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("text = %v", body["text"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	toolEntry, _ := tools[0].(map[string]any)
	if toolEntry["type"] != "function" || toolEntry["name"] != "lookup" {
		t.Fatalf("tool = %v", tools[0])
	}
	params, _ := toolEntry["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters = %v", toolEntry["parameters"])
	}
	if choice, _ := body["tool_choice"].(string); choice != "auto" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}

	input, _ := body["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input items = %d", len(input))
	}
	assistant, _ := input[0].(map[string]any)
	if assistant["type"] != "message" || assistant["role"] != "assistant" {
		t.Fatalf("assistant item = %v", assistant)
	}
	contents, _ := assistant["content"].([]any)
	if len(contents) != 1 {
		t.Fatalf("assistant content = %v", assistant["content"])
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" ||
		call["name"] != "lookup" || call["arguments"] != `{"q":"x"}` {
		t.Fatalf("call item = %v", call)
	}
	output, _ := input[2].(map[string]any)
	if output["type"] != "function_call_output" || output["output"] != "result text" {
		t.Fatalf("output item = %v", output)
	}
	user, _ := input[3].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Fatalf("user item = %v", user)
	}
	userContent, _ := user["content"].([]any)
	first, _ := userContent[0].(map[string]any)
	if first["type"] != "input_text" || first["text"] != "current question" {
		t.Fatalf("user content = %v", user["content"])
	}
}
