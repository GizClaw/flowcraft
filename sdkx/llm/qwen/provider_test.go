package qwen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
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

func TestNewUsesDashScopeResponsesBaseURL(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/apps/protocols/compatible-mode/v1/responses" {
			t.Fatalf("path = %q, want DashScope Responses path", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":0,"model":"qwen-flash","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := New("qwen-flash", "test-key", srv.URL+"/api/v2/apps/protocols/compatible-mode/v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "ok" || got["model"] != "qwen-flash" {
		t.Fatalf("content=%q body=%#v", msg.Content(), got)
	}
}

func TestResponsesBaseURLNormalization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty uses China default",
			in:   "",
			want: defaultResponsesBaseURL,
		},
		{
			name: "China chat base switches to responses base",
			in:   "https://dashscope.aliyuncs.com/compatible-mode/v1",
			want: defaultResponsesBaseURL,
		},
		{
			name: "international chat base switches to responses base",
			in:   "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
			want: "https://dashscope-intl.aliyuncs.com/api/v2/apps/protocols/compatible-mode/v1",
		},
		{
			name: "responses base stays unchanged",
			in:   "https://dashscope-intl.aliyuncs.com/api/v2/apps/protocols/compatible-mode/v1/",
			want: "https://dashscope-intl.aliyuncs.com/api/v2/apps/protocols/compatible-mode/v1",
		},
		{
			name: "custom base stays explicit",
			in:   "https://example.test/custom/v1",
			want: "https://example.test/custom/v1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := qwenResponsesBaseURL(tc.in); got != tc.want {
				t.Fatalf("qwenResponsesBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewMapsThinkingToResponsesEnableThinking(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []llm.GenerateOption
		want bool
	}{
		{name: "default disabled", want: false},
		{name: "enabled", opts: []llm.GenerateOption{llm.WithThinking(true)}, want: true},
		{name: "disabled", opts: []llm.GenerateOption{llm.WithThinking(false)}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":0,"model":"qwen-flash","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			}))
			defer srv.Close()

			c, err := New("qwen-flash", "test-key", srv.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}, tc.opts...); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got["enable_thinking"] != tc.want {
				t.Fatalf("enable_thinking = %#v, want %v; body=%#v", got["enable_thinking"], tc.want, got)
			}
		})
	}
}

func TestQwenChatProviderIsNotRegistered(t *testing.T) {
	_, err := llm.NewFromConfig("qwen-chat", "qwen-flash", map[string]any{"api_key": "test-key"})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("qwen-chat registration error = %v, want NotFound", err)
	}
}
