package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: deepseek
// must call openai.LLM.WithProviderName so traces/metrics from DeepSeek
// traffic land under "deepseek" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("deepseek-chat", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want deepseek", got)
	}
}

// TestNewDefaultsToCurrentSKU checks that the zero-config default model
// is the current non-deprecated SKU, not the legacy deepseek-chat alias
// that is scheduled for hard retirement on 2026-07-24.
func TestNewDefaultsToCurrentSKU(t *testing.T) {
	var gotModel string
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			sawAuth = true
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"test",
			"object":"chat.completion",
			"created":1,
			"model":"` + req.Model + `",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer srv.Close()

	c, err := New("", "k", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want deepseek", got)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hello")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !sawAuth {
		t.Fatalf("Authorization header missing")
	}
	if gotModel != "deepseek-v4-flash" {
		t.Fatalf("model on wire = %q, want deepseek-v4-flash", gotModel)
	}
}
