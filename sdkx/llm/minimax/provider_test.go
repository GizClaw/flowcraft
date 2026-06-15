package minimax

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

// TestNewTagsProvider locks down the sub-provider plumbing: minimax
// must call anthropic.LLM.WithProviderName so traces/metrics from
// MiniMax traffic land under "minimax" rather than the upstream
// "anthropic" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("MiniMax-M2.7-highspeed", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "minimax" {
		t.Fatalf("Provider() = %q, want minimax", got)
	}
}

func TestNewOpenAITagsProvider(t *testing.T) {
	c, err := NewOpenAI("MiniMax-M2.7-highspeed", "k", "")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if got := c.Provider(); got != "minimax-oai" {
		t.Fatalf("Provider() = %q, want minimax-oai", got)
	}
}

func TestOpenAICompatibleProviderRegistered(t *testing.T) {
	c, err := llm.NewFromConfig("minimax-oai", "MiniMax-M2.7-highspeed", map[string]any{
		"api_key": "k",
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	tagged, ok := c.(interface{ Provider() string })
	if !ok {
		t.Fatalf("client does not expose Provider()")
	}
	if got := tagged.Provider(); got != "minimax-oai" {
		t.Fatalf("Provider() = %q, want minimax-oai", got)
	}
}

func TestOpenAICompatibleProviderUsesResponses(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":0,"model":"MiniMax-M3","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := llm.NewFromConfig("minimax-oai", "", map[string]any{
		"api_key":  "test-key",
		"base_url": srv.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	msg, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}, llm.WithThinking(true))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want object; body=%#v", got["reasoning"], got)
	}
	if reasoning["effort"] != "minimal" {
		t.Fatalf("reasoning.effort = %#v, want minimal; body=%#v", reasoning["effort"], got)
	}
	if _, ok := got["reasoning_split"]; ok {
		t.Fatalf("reasoning_split should not be sent on Responses: %#v", got)
	}
	if msg.Content() != "ok" || got["model"] != defaultOpenAIModel {
		t.Fatalf("content=%q body=%#v", msg.Content(), got)
	}
}

func TestOpenAICompatibleProviderThinkingFalseDisablesResponsesReasoning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":0,"model":"MiniMax-M2.7-highspeed","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := NewOpenAI("MiniMax-M2.7-highspeed", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if _, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}, llm.WithThinking(false)); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "none" {
		t.Fatalf("reasoning = %#v, want effort none; body=%#v", got["reasoning"], got)
	}
}

func TestOpenAICompatibleProviderRejectsUnsupportedToolChoice(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "request should not be sent for unsupported tool_choice", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := NewOpenAI("MiniMax-M3", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	msgs := []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}
	for _, tc := range []struct {
		name string
		opt  llm.GenerateOption
	}{
		{name: "required", opt: llm.WithToolChoiceRequired()},
		{name: "specific", opt: llm.WithToolChoiceSpecific("lookup")},
	} {
		t.Run(tc.name+"/generate", func(t *testing.T) {
			_, _, err := c.Generate(context.Background(), msgs, tc.opt)
			if !errdefs.IsValidation(err) {
				t.Fatalf("Generate error = %v, want Validation", err)
			}
		})
		t.Run(tc.name+"/stream", func(t *testing.T) {
			_, err := c.GenerateStream(context.Background(), msgs, tc.opt)
			if !errdefs.IsValidation(err) {
				t.Fatalf("GenerateStream error = %v, want Validation", err)
			}
		})
	}
	if called {
		t.Fatal("server was called for unsupported tool_choice")
	}
}

func TestOpenAICompatibleChatProviderIsNotRegistered(t *testing.T) {
	_, err := llm.NewFromConfig("minimax-oai-chat", "MiniMax-M3", map[string]any{"api_key": "test-key"})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("minimax-oai-chat registration error = %v, want NotFound", err)
	}
}

func TestOpenAICompatibleCatalogDisablesJSONFormatting(t *testing.T) {
	spec := llm.DefaultRegistry.LookupModelSpec("minimax-oai", "MiniMax-M3")
	if spec.Caps.Supports(llm.CapJSONSchema) {
		t.Fatal("minimax-oai MiniMax-M3 must not advertise JSON schema support")
	}
	if spec.Caps.Supports(llm.CapJSONMode) {
		t.Fatal("minimax-oai MiniMax-M3 must not advertise JSON mode support")
	}
}

func TestOpenAICompatibleCatalogDoesNotDefaultReasoningSplit(t *testing.T) {
	spec := llm.DefaultRegistry.LookupModelSpec("minimax-oai", "MiniMax-M3")
	if _, ok := spec.Defaults.Extra["reasoning_split"]; ok {
		t.Fatalf("reasoning_split default = %v, want absent", spec.Defaults.Extra["reasoning_split"])
	}
}

func TestOpenAICompatibleCatalogIncludesMiniMaxM3(t *testing.T) {
	info, ok := llm.DefaultRegistry.LookupModel("minimax-oai", "MiniMax-M3")
	if !ok {
		t.Fatal("minimax-oai catalog must include MiniMax-M3")
	}
	if info.Name != defaultOpenAIModel {
		t.Fatalf("catalog model = %q, want %q", info.Name, defaultOpenAIModel)
	}
}
