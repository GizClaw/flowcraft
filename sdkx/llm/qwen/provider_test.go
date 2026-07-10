package qwen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: the qwen
// wrapper must call openai.LLM.WithProviderName so traces/metrics from
// Qwen traffic land under "qwen" rather than the upstream "openai" tag.
// Regressing this silently aggregates Qwen QPS into the openai bucket,
// which is exactly the observability bug this plumbing exists to fix.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("qwen-flash", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.inner.Provider(); got != "qwen" {
		t.Fatalf("Provider() = %q, want qwen", got)
	}
}

// TestQwenExtrasOnWire verifies that Qwen-specific options are mapped to
// the legacy/extra body fields the DashScope OpenAI-compatible endpoint
// actually honors: max_tokens (not max_completion_tokens), top_k, and
// enable_thinking.
func TestQwenExtrasOnWire(t *testing.T) {
	var reqBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &reqBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"test",
			"object":"chat.completion",
			"created":1,
			"model":"qwen-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer srv.Close()

	maxTokens := int64(300)
	topK := int64(50)
	thinking := true
	c, err := New("qwen-flash", "k", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hello")},
		llm.WithMaxTokens(maxTokens),
		llm.WithTopK(topK),
		llm.WithThinking(thinking),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got, ok := reqBody["max_tokens"]; !ok || got != float64(maxTokens) {
		t.Fatalf("max_tokens on wire = %v (%v), want %v", got, ok, maxTokens)
	}
	if got, ok := reqBody["top_k"]; !ok || got != float64(topK) {
		t.Fatalf("top_k on wire = %v (%v), want %v", got, ok, topK)
	}
	if got, ok := reqBody["enable_thinking"]; !ok || got != true {
		t.Fatalf("enable_thinking on wire = %v (%v), want true", got, ok)
	}
}
