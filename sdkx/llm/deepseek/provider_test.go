package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: deepseek
// must call openai.LLM.WithProviderName so traces/metrics from DeepSeek
// traffic land under "deepseek" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("deepseek-v4-flash", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want deepseek", got)
	}
}

func TestProviderNilReceiverSafe(t *testing.T) {
	var c *LLM
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("nil receiver Provider() = %q, want deepseek", got)
	}
}

func TestCatalogDefaultsDisableThinking(t *testing.T) {
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		spec := llm.DefaultRegistry.LookupModelSpec("deepseek", model)
		thinking, ok := spec.Defaults.Extra["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("%s thinking default = %#v, want object", model, spec.Defaults.Extra["thinking"])
		}
		if got := thinking["type"]; got != "disabled" {
			t.Fatalf("%s thinking.type = %#v, want disabled", model, got)
		}
	}
}

func TestDeepSeekResponsesProviderNotRegistered(t *testing.T) {
	if _, err := llm.NewFromConfig("deepseek-responses", "deepseek-v4-flash", nil); err == nil {
		t.Fatal("deepseek-responses should not be registered")
	}
}

func TestGenerate_WithThinkingMapsToDeepSeekBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  llm.GenerateOption
		want string
	}{
		{name: "enabled", opt: llm.WithThinking(true), want: "enabled"},
		{name: "disabled", opt: llm.WithThinking(false), want: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, captured := newDeepSeekCaptureClient(t)
			_, _, err := c.Generate(context.Background(), []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "hi"),
			}, tc.opt)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := readDeepSeekCapturedBody(t, captured)
			thinking, ok := body["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking body field = %#v, want object; body=%#v", body["thinking"], body)
			}
			if got := thinking["type"]; got != tc.want {
				t.Fatalf("thinking.type = %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestGenerate_CatalogDefaultDisablesDeepSeekThinkingOnWire(t *testing.T) {
	c, captured := newDeepSeekCaptureClient(t)
	spec := llm.DefaultRegistry.LookupModelSpec("deepseek", "deepseek-v4-flash")
	wrapped := llm.WithDefaults(c, spec.Defaults)
	_, _, err := wrapped.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := readDeepSeekCapturedBody(t, captured)
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking body field = %#v, want object; body=%#v", body["thinking"], body)
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %#v, want disabled", got)
	}
}

func newDeepSeekCaptureClient(t *testing.T) (llm.LLM, <-chan map[string]any) {
	t.Helper()
	captured := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode request body: %v\nbody=%s", err, raw)
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New("deepseek-v4-flash", "k", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, captured
}

func readDeepSeekCapturedBody(t *testing.T, captured <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case body := <-captured:
		return body
	case <-time.After(time.Second):
		t.Fatal("server did not capture request body")
	}
	return nil
}
