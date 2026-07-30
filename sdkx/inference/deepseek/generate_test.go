package deepseek

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func simpleTextRequest(text string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: inference.Content{
					Parts: []inference.Part{inference.TextPart{Text: text}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
}

func generateModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "deepseek", Name: name},
		Profile: "default",
	}
}

// chatServer is a fake chat completions endpoint: it captures the request
// JSON and answers with the handler's fixture (unary object or SSE body).
type chatServer struct {
	*httptest.Server
	mu      sync.Mutex
	request map[string]any
}

func newChatServer(t *testing.T, handler func(w http.ResponseWriter, request map[string]any)) *chatServer {
	t.Helper()
	captured := &chatServer{}
	captured.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured.mu.Lock()
		captured.request = request
		captured.mu.Unlock()
		handler(w, request)
	}))
	t.Cleanup(captured.Close)
	return captured
}

func (s *chatServer) captured() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request
}

func (s *chatServer) clients(t *testing.T) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(fmt.Sprintf(`{"base_url":%q}`, s.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

// chatCompletionJSON renders a unary completion fixture. Extra message
// fields (reasoning_content) ride the messageExtras map.
func chatCompletionJSON(finish string, messageExtras map[string]any) string {
	message := map[string]any{"role": "assistant", "content": "ok"}
	for key, value := range messageExtras {
		message[key] = value
	}
	payload, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl_1",
		"object":  "chat.completion",
		"created": 1,
		"model":   "deepseek-v4-pro",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": finish,
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

// sseBody renders a chat completions SSE fixture: one data line per chunk,
// terminated by [DONE].
func sseBody(chunks ...map[string]any) string {
	body := ""
	for _, chunk := range chunks {
		payload, _ := json.Marshal(chunk)
		body += "data: " + string(payload) + "\n\n"
	}
	return body + "data: [DONE]\n\n"
}

func textChunk(text string) map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1,
		"model": "deepseek-v4-pro",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": nil,
			"delta": map[string]any{"content": text},
		}},
	}
}

func reasoningChunk(text string) map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1,
		"model": "deepseek-v4-pro",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": nil,
			"delta": map[string]any{"reasoning_content": text},
		}},
	}
}

func finishChunk(reason string) map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1,
		"model": "deepseek-v4-pro",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": reason,
			"delta": map[string]any{},
		}},
	}
}

func usageChunk() map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1,
		"model":   "deepseek-v4-pro",
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":             12,
			"completion_tokens":         7,
			"total_tokens":              19,
			"prompt_cache_hit_tokens":   3,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	}
}

func toolCallDeltaChunk(index int, id, name, argsFragment string) map[string]any {
	call := map[string]any{
		"index": index, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": argsFragment},
	}
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1,
		"model": "deepseek-v4-pro",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": nil,
			"delta": map[string]any{"tool_calls": []map[string]any{call}},
		}},
	}
}

func toolCallDefinition() tool.Definition {
	return tool.Definition{
		Name:        "lookup",
		Description: "find things",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
}
