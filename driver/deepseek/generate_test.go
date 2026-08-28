package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func simpleTextRequest(text string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: text}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
}

func deepseekModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "deepseek", Name: name},
		Profile: "default",
	}
}

func testClients(t *testing.T, server *httptest.Server) *clients {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(`{"base_url":%q}`, server.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

// capturedServer records the last request body and delegates response
// writing to the handler.
type capturedServer struct {
	*httptest.Server
	mu      sync.Mutex
	request map[string]any
}

func newCapturedServer(
	t *testing.T,
	handler func(w http.ResponseWriter, body map[string]any),
) *capturedServer {
	t.Helper()
	captured := &capturedServer{}
	captured.Server = httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured.mu.Lock()
		captured.request = body
		captured.mu.Unlock()
		handler(w, body)
	}))
	t.Cleanup(captured.Close)
	return captured
}

func (s *capturedServer) captured() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request
}

func chatEntry() catalogEntry {
	return catalogEntry{
		kind:         kindGenerate,
		api:          apiChat,
		capabilities: generateChatCapabilities().WithReasoning(inference.ReasoningToggle),
	}
}

func responsesEntry() catalogEntry {
	return catalogEntry{
		kind:         kindGenerate,
		api:          apiResponses,
		capabilities: generateChatCapabilities().WithHostedWebSearch().WithReasoning(inference.ReasoningToggle),
		responses:    true,
	}
}

func chatCompletionJSON(reasoning string) string {
	message := map[string]any{"role": "assistant", "content": "ok"}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	payload, _ := json.Marshal(map[string]any{
		"id":     "chatcmpl_1",
		"object": "chat.completion",
		"model":  "deepseek-v4-flash",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message":       message,
		}},
		"usage": map[string]any{
			"prompt_tokens":             12,
			"completion_tokens":         7,
			"total_tokens":              19,
			"prompt_cache_hit_tokens":   3,
			"prompt_cache_miss_tokens":  9,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	})
	return string(payload)
}

func responsesResponseJSON(outputs []map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id":     "resp_1",
		"object": "response",
		"status": "completed",
		"model":  "deepseek-v4-flash",
		"output": outputs,
		"usage": map[string]any{
			"input_tokens":          22,
			"output_tokens":         29,
			"total_tokens":          51,
			"input_tokens_details":  map[string]any{"cached_tokens": 3},
			"output_tokens_details": map[string]any{"reasoning_tokens": 27},
		},
	})
	return string(payload)
}

func sseBody(events ...map[string]any) string {
	body := ""
	for _, event := range events {
		payload, _ := json.Marshal(event)
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

func textOutputItem(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"id":   "msg_1",
		"role": "assistant",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
}

func reasoningOutputItem(text string) map[string]any {
	return map[string]any{
		"type": "reasoning",
		"id":   "rs_1",
		"content": []map[string]any{{
			"type": "reasoning_text",
			"text": text,
		}},
	}
}

func TestChatUnaryReasoning(t *testing.T) {
	server := newCapturedServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatCompletionJSON("thinking aloud"))
	})
	cls := testClients(t, server.Server)
	operations, err := openGenerate(cls, chatEntry(), deepseekModel("deepseek-v4-flash").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	response, err := operations.Unary.Execute(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d (%#v)", len(parts), parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "thinking aloud" {
		t.Fatalf("part[0] = %#v", parts[0])
	}
	if _, ok := parts[1].(message.TextPart); !ok {
		t.Fatalf("part[1] = %#v", parts[1])
	}
	if response.Usage.Input.CacheReadTokens == nil ||
		*response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cache = %+v", response.Usage.Input)
	}
	if response.Usage.Input.UncachedTokens == nil ||
		*response.Usage.Input.UncachedTokens != 9 {
		t.Fatalf("uncached = %+v", response.Usage.Input)
	}
	if response.Usage.Output.ReasoningTokens == nil ||
		*response.Usage.Output.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %+v", response.Usage.Output)
	}
	if response.Metadata.ResponseID != "chatcmpl_1" {
		t.Fatalf("response id = %q", response.Metadata.ResponseID)
	}
}

func TestChatReasoningRoundTrip(t *testing.T) {
	server := newCapturedServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatCompletionJSON(""))
	})
	cls := testClients(t, server.Server)
	operations, err := openGenerate(cls, chatEntry(), deepseekModel("deepseek-v4-flash").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}

	request := simpleTextRequest("again")
	request.Context = []message.Message{
		{
			Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "find something"},
			}},
		},
		{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ReasoningPart{Text: "trace"},
				message.ToolCallPart{Call: message.ToolCall{
					ID:        "call_9",
					Name:      "lookup",
					Arguments: json.RawMessage(`{"q":"ark"}`),
				}},
			}},
		},
		{
			Role: message.RoleTool,
			Content: message.Content{Parts: []message.Part{
				message.ToolResultPart{Result: message.ToolResult{
					CallID:  "call_9",
					Content: "found it",
				}},
			}},
		},
	}
	if _, err := operations.Unary.Execute(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	messages, _ := server.captured()["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("wire messages = %d", len(messages))
	}
	assistant, _ := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "trace" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	toolMessage, _ := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_9" {
		t.Fatalf("tool message = %v", toolMessage)
	}
}

func TestResponsesUnaryReasoningAndText(t *testing.T) {
	server := newCapturedServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			reasoningOutputItem("thinking aloud"),
			textOutputItem("answer"),
		}))
	})
	cls := testClients(t, server.Server)
	operations, err := openGenerate(cls, responsesEntry(), deepseekModel("deepseek-v4-flash").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	response, err := operations.Unary.Execute(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d (%#v)", len(parts), parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "thinking aloud" || reasoning.ID != "rs_1" {
		t.Fatalf("reasoning part = %#v", parts[0])
	}
	text, ok := parts[1].(message.TextPart)
	if !ok || text.Text != "answer" {
		t.Fatalf("text part = %#v", parts[1])
	}
	if response.Metadata.ResponseID != "resp_1" {
		t.Fatalf("response id = %q", response.Metadata.ResponseID)
	}
	if response.Usage.Input.CacheReadTokens == nil ||
		*response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cache = %+v", response.Usage.Input)
	}
}

func TestResponsesStreamReasoningText(t *testing.T) {
	server := newCapturedServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "reasoning", "id": "rs_1"},
			},
			map[string]any{
				"type":         "response.reasoning_text.delta",
				"output_index": 0, "item_id": "rs_1", "delta": "think",
			},
			map[string]any{
				"type":         "response.reasoning_text.delta",
				"output_index": 0, "item_id": "rs_1", "delta": "ing",
			},
			map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": reasoningOutputItem("thinking"),
			},
			map[string]any{
				"type": "response.output_item.added", "output_index": 1,
				"item": map[string]any{"type": "message"},
			},
			map[string]any{
				"type":         "response.output_text.delta",
				"output_index": 1, "delta": "done",
			},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp_1", "status": "completed",
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
						"input_tokens_details":  map[string]any{"cached_tokens": 0},
						"output_tokens_details": map[string]any{"reasoning_tokens": 1},
					},
				},
			},
		))
	})
	cls := testClients(t, server.Server)
	operations, err := openGenerate(cls, responsesEntry(), deepseekModel("deepseek-v4-flash").ID, "default")
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	stream, err := operations.Stream.Stream(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var reasoning, text string
	var finish inference.FinishReason
	var usage *inference.Usage
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
		case inference.TextPartDelta:
			text += delta.Text
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if reasoning != "thinking" || text != "done" {
		t.Fatalf("stream = reasoning %q text %q", reasoning, text)
	}
	if finish != inference.FinishCompleted {
		t.Fatalf("finish = %q", finish)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestResponsesWebSearchCompilesWithoutInclude(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{
		GenerateOptions{
			WebSearch: &GenerateWebSearch{
				SearchContextSize: "high",
				AllowedDomains:    []string{"example.com"},
				ToolChoice:        &GenerateWebSearchToolChoice{Required: true},
			},
		},
	}
	compiled, err := compileResponsesGenerate("deepseek-v4-flash", responsesEntry())(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	params := wireToResponseParams(compiled.Wire)
	if len(params.Tools) != 1 || params.Tools[0].OfWebSearch == nil {
		t.Fatalf("tools = %+v, want one hosted web_search tool", params.Tools)
	}
	if params.ToolChoice.OfToolChoiceMode.Value != "required" {
		t.Fatalf("tool choice = %+v, want required", params.ToolChoice)
	}
	if len(params.Include) != 0 {
		t.Fatalf("include = %v, deepseek does not support include", params.Include)
	}
}

func TestResponsesJSONSchemaCompiles(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text.Response = &inference.ResponseFormat{
		Kind:   inference.ResponseJSONSchema,
		Name:   "answer",
		Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}
	compiled, err := compileResponsesGenerate("deepseek-v4-flash", responsesEntry())(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	params := wireToResponseParams(compiled.Wire)
	if params.Text.Format.OfJSONSchema == nil {
		t.Fatalf("text format = %+v, want json_schema", params.Text.Format)
	}
}
