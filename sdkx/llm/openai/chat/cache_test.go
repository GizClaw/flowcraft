package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestBuildParams_InjectsPromptCacheKey is the end-to-end wire-level
// check: when a request has a stable system prompt, the rendered
// chat-completions request body contains a non-empty
// `prompt_cache_key` field. Without this the routing hint is lost
// even if ComputePromptCacheKey works.
func TestBuildParams_InjectsPromptCacheKey(t *testing.T) {
	var captured []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{
			"id": "x",
			"model": "gpt-test",
			"object": "chat.completion",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`)
	}))
	defer ts.Close()

	c, _ := New("gpt-test", "k", ts.URL)
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "stable persona"),
			llm.NewTextMessage(llm.RoleUser, "first call"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var body struct {
		PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, captured)
	}
	if body.PromptCacheKey == "" {
		t.Fatalf("expected prompt_cache_key in request body, got empty\nbody=%s", captured)
	}
	if len(body.PromptCacheKey) != 16 {
		t.Errorf("expected 16-hex-char key, got %d chars: %q", len(body.PromptCacheKey), body.PromptCacheKey)
	}
}

// TestBuildParams_NoCacheKeyWhenNothingStable: a call with no system
// messages and no tools should NOT emit a prompt_cache_key - sending
// an empty / pointless value would just pollute the backend's
// routing table.
func TestBuildParams_NoCacheKeyWhenNothingStable(t *testing.T) {
	var captured []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{
			"id":"x","model":"gpt-test","object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer ts.Close()

	c, _ := New("gpt-test", "k", ts.URL)
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "just a user msg")},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Decode the body and check field is absent / empty.
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, ok := body["prompt_cache_key"]; ok && v != "" {
		t.Fatalf("expected no prompt_cache_key, got %v", v)
	}
}
