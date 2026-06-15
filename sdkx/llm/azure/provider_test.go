package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: azure must
// call openai.LLM.WithProviderName so traces/metrics from Azure-routed
// traffic land under "azure" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("gpt-5", "k", "https://example.openai.azure.com", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "azure" {
		t.Fatalf("Provider() = %q, want azure", got)
	}
}

func TestNewUsesChatCompletionsEndpoint(t *testing.T) {
	var gotPath, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.URL.Query().Get("api-version")
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Fatalf("path = %q, want chat completions path", r.URL.Path)
		}
		if gotVersion != defaultAPIVersion {
			t.Fatalf("api-version = %q, want %q", gotVersion, defaultAPIVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := New("deployment", "test-key", srv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "ok" || !strings.Contains(gotPath, "/chat/completions") || gotVersion == "" {
		t.Fatalf("content=%q path=%q api-version=%q", msg.Content(), gotPath, gotVersion)
	}
}
