package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdk/message"
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

func minimaxModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "minimax", Name: name},
		Profile: "default",
	}
}

// messagesServer is a fake Anthropic Messages endpoint: it captures the
// request JSON and answers with the handler's fixture.
type messagesServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newMessagesServer(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *messagesServer {
	t.Helper()
	captured := &messagesServer{}
	captured.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		captured.mu.Lock()
		captured.bodies = append(captured.bodies, body)
		captured.mu.Unlock()
		// Default to JSON; streaming handlers override with event-stream.
		w.Header().Set("Content-Type", "application/json")
		handler(w, body)
	}))
	t.Cleanup(captured.Close)
	return captured
}

func (s *messagesServer) requests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.bodies))
}

func (s *messagesServer) body(t *testing.T, index int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.bodies) {
		t.Fatalf("only %d captured requests", len(s.bodies))
	}
	return s.bodies[index]
}

func (s *messagesServer) clients(t *testing.T) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(fmt.Sprintf(`{"base_url":%q}`, s.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

// messageJSON renders a Messages response fixture.
func messageJSON(content []map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id":            "msg_1",
		"type":          "message",
		"role":          "assistant",
		"model":         "MiniMax-M3",
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  12,
			"output_tokens": 7,
		},
	})
	return string(payload)
}

// sseBody renders a Messages SSE fixture: event line plus data line.
func sseBody(events ...map[string]any) string {
	body := ""
	for _, event := range events {
		payload, _ := json.Marshal(event)
		eventType, _ := event["type"].(string)
		body += "event: " + eventType + "\n"
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

func textStreamBody(text string) string {
	return sseBody(
		map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": "MiniMax-M3", "content": []any{},
				"usage": map[string]any{"input_tokens": 12, "output_tokens": 0},
			},
		},
		map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		},
		map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"output_tokens": 7},
		},
		map[string]any{"type": "message_stop"},
	)
}

func testSecret(t *testing.T, value string) config.Secret {
	t.Helper()
	secret, err := config.NewSecret([]byte(value))
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	return secret
}

func buildProvider(t *testing.T, spec map[string]any) inference.ProviderDefinition {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "minimax",
		Spec: raw,
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{SecretAPIKey: testSecret(t, "test-key")},
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return provider
}

func newTestRuntime(t *testing.T, server *messagesServer) *inference.Runtime {
	t.Helper()
	provider := buildProvider(t, map[string]any{"base_url": server.URL})
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{provider})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

// ---------------------------------------------------------------------------
// Spec, profile, factory.
// ---------------------------------------------------------------------------

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "empty ok", spec: `{}`},
		{name: "base url ok", spec: `{"base_url":"https://api.minimax.io/anthropic"}`},
		{name: "base url not http", spec: `{"base_url":"minimax.io"}`, wantErr: "http(s)"},
		{name: "model ok", spec: `{"models":[{"name":"my-model","kind":"generate","reasoning":true}]}`},
		{name: "model bad name", spec: `{"models":[{"name":"-bad"}]}`, wantErr: "invalid model name"},
		{name: "model bad kind", spec: `{"models":[{"name":"m","kind":"embed"}]}`, wantErr: "unsupported kind"},
		{name: "duplicate model", spec: `{"models":[{"name":"m"},{"name":"m"}]}`, wantErr: "duplicate"},
		{name: "unknown field", spec: `{"api_key":"x"}`, wantErr: "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec([]byte(tc.spec))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("decodeSpec: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestFactoryBuildsCatalog(t *testing.T) {
	provider := buildProvider(t, nil)
	if provider.ID != "minimax" {
		t.Fatalf("provider id = %q", provider.ID)
	}
	if len(provider.Models) != len(catalog) {
		t.Fatalf("models = %d, want %d", len(provider.Models), len(catalog))
	}
	names := make(map[string]bool, len(provider.Models))
	for _, model := range provider.Models {
		names[model.Descriptor.ID.Name] = true
		if model.Openers.Generate == nil {
			t.Fatalf("model %q has no generate opener", model.Descriptor.ID.Name)
		}
	}
	if !names["MiniMax-M3"] || !names["MiniMax-M2.7"] {
		t.Fatalf("catalog names = %v", names)
	}
}

func TestFactoryRejectsBadSecrets(t *testing.T) {
	raw := []byte(`{}`)
	_, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "minimax",
		Spec: raw,
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{"bogus": testSecret(t, "x")},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown secret") {
		t.Fatalf("err = %v", err)
	}

	_, err = Factory().Build(context.Background(), config.ProviderInput{
		ID:       "minimax",
		Spec:     raw,
		Profiles: []config.ResolvedProfile{{ID: "default"}},
	})
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err = %v", err)
	}
}
