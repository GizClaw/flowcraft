package kimi

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
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
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

func kimiModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "kimi", Name: name},
		Profile: "default",
	}
}

// kimiServer is a fake chat completions endpoint: it captures the request
// path, streaming flag, and JSON body, then answers with the handler's
// fixture.
type kimiServer struct {
	*httptest.Server
	mu      sync.Mutex
	paths   []string
	streams []bool
	bodies  []map[string]any
}

func newKimiServer(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *kimiServer {
	t.Helper()
	captured := &kimiServer{}
	captured.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var body map[string]any
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("body is not JSON: %v\n%s", err, payload)
				return
			}
		}
		captured.mu.Lock()
		captured.paths = append(captured.paths, r.URL.Path)
		captured.streams = append(captured.streams, r.Header.Get("Accept") == "text/event-stream")
		captured.bodies = append(captured.bodies, body)
		captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handler(w, body)
	}))
	t.Cleanup(captured.Close)
	return captured
}

func (s *kimiServer) requests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.bodies))
}

func (s *kimiServer) body(t *testing.T, index int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.bodies) {
		t.Fatalf("only %d captured requests", len(s.bodies))
	}
	return s.bodies[index]
}

func (s *kimiServer) path(t *testing.T, index int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.paths) {
		t.Fatalf("only %d captured requests", len(s.paths))
	}
	return s.paths[index]
}

func (s *kimiServer) streaming(t *testing.T, index int) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.streams) {
		t.Fatalf("only %d captured requests", len(s.streams))
	}
	return s.streams[index]
}

func (s *kimiServer) client(t *testing.T) *kimiClient {
	t.Helper()
	spec, err := decodeSpec([]byte(fmt.Sprintf(`{"base_url":%q}`, s.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClient(spec)
}

// bindOps wires the generate drivers for one catalog model against the
// fake server.
func (s *kimiServer) bindOps(t *testing.T, model string) inference.GenerateOperations {
	t.Helper()
	ops, err := inference.BindGenerateOperations(
		compileGenerate(model, catalog[model]),
		transportGenerate(s.client(t)),
		decodeGenerate,
		transportGenerateStream(s.client(t)),
		decodeGenerateStream,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return ops
}

// completionEnvelope renders a unary response fixture.
func completionBody(message map[string]any, finish string) string {
	payload, _ := json.Marshal(map[string]any{
		"id":     "cmpl-1",
		"object": "chat.completion",
		"model":  "test",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     19,
			"completion_tokens": 21,
			"total_tokens":      40,
			"cached_tokens":     10,
		},
	})
	return string(payload)
}

func textCompletion(text string) string {
	return completionBody(map[string]any{
		"role":    "assistant",
		"content": text,
	}, "stop")
}

// chunkBody renders an SSE fixture from chunk objects.
func chunkBody(chunks ...map[string]any) string {
	body := ""
	for _, chunk := range chunks {
		payload, _ := json.Marshal(chunk)
		body += "data: " + string(payload) + "\n\n"
	}
	return body + "data: [DONE]\n\n"
}

// streamChunk builds one streaming chunk: delta fields plus an optional
// finish reason and usage on the last chunk.
func streamChunk(delta map[string]any, finish any, usage bool) map[string]any {
	chunk := map[string]any{
		"id":     "cmpl-1",
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     19,
			"completion_tokens": 13,
			"total_tokens":      32,
			"cached_tokens":     12,
		}
	}
	return chunk
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
		ID:   "kimi",
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

func newTestRuntime(t *testing.T, server *kimiServer) *inference.Runtime {
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
		{name: "base url ok", spec: `{"base_url":"https://api.moonshot.cn/v1"}`},
		{name: "base url not http", spec: `{"base_url":"api.moonshot.cn"}`, wantErr: "http(s)"},
		{name: "model ok", spec: `{"models":[{"name":"kimi-k9","kind":"generate","vision":true,"reasoning":true}]}`},
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
	if provider.ID != "kimi" {
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
	if !names["kimi-k3"] || !names["kimi-k2.6"] || !names["moonshot-v1-8k"] {
		t.Fatalf("catalog names = %v", names)
	}
}

func TestFactoryRejectsBadSecrets(t *testing.T) {
	raw := []byte(`{}`)
	_, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "kimi",
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
		ID:       "kimi",
		Spec:     raw,
		Profiles: []config.ResolvedProfile{{ID: "default"}},
	})
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err = %v", err)
	}
}
