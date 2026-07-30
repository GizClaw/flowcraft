package qwen

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
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
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

func qwenModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "qwen", Name: name},
		Profile: "default",
	}
}

// dashServer is a fake DashScope generation endpoint: it captures the
// request path, SSE header, and JSON body, then answers with the
// handler's fixture.
type dashServer struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	sse    []bool
	bodies []map[string]any
}

func newDashServer(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *dashServer {
	t.Helper()
	captured := &dashServer{}
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
		captured.paths = append(captured.paths, r.URL.Path)
		captured.sse = append(captured.sse, r.Header.Get("X-DashScope-SSE") == "enable")
		captured.bodies = append(captured.bodies, body)
		captured.mu.Unlock()
		// Default to JSON; streaming handlers override with event-stream.
		w.Header().Set("Content-Type", "application/json")
		handler(w, body)
	}))
	t.Cleanup(captured.Close)
	return captured
}

func (s *dashServer) requests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.bodies))
}

func (s *dashServer) body(t *testing.T, index int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.bodies) {
		t.Fatalf("only %d captured requests", len(s.bodies))
	}
	return s.bodies[index]
}

func (s *dashServer) path(t *testing.T, index int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.paths) {
		t.Fatalf("only %d captured requests", len(s.paths))
	}
	return s.paths[index]
}

func (s *dashServer) streaming(t *testing.T, index int) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.sse) {
		t.Fatalf("only %d captured requests", len(s.sse))
	}
	return s.sse[index]
}

func (s *dashServer) client(t *testing.T) *dashClient {
	t.Helper()
	spec, err := decodeSpec([]byte(fmt.Sprintf(`{"base_url":%q}`, s.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClient(spec)
}

// bindOps wires the generate drivers for one catalog model against the
// fake server.
func (s *dashServer) bindOps(t *testing.T, model string) inference.GenerateOperations {
	t.Helper()
	ops, err := inference.BindGenerateOperations(
		compileGenerate(model, catalog[model]),
		transportGenerate(s.client(t)),
		decodeGenerate,
		transportGenerateStream(s.client(t)),
		decodeStreamFragment,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return ops
}

// dashEnvelope renders a generation envelope fixture.
func dashEnvelope(message map[string]any, finish string) string {
	payload, _ := json.Marshal(map[string]any{
		"status_code": 200,
		"request_id":  "req_1",
		"code":        "",
		"message":     "",
		"output": map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": finish,
				"message":       message,
			}},
		},
		"usage": map[string]any{
			"input_tokens":  12,
			"output_tokens": 7,
			"total_tokens":  19,
		},
	})
	return string(payload)
}

func textEnvelope(text string) string {
	return dashEnvelope(map[string]any{
		"role":    "assistant",
		"content": text,
	}, "stop")
}

// dashSSEBody renders an SSE fixture from envelope chunks.
func dashSSEBody(chunks ...map[string]any) string {
	body := ""
	for _, chunk := range chunks {
		payload, _ := json.Marshal(chunk)
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

// streamChunk builds one streaming envelope chunk: delta content plus an
// optional finish reason and usage on the last chunk.
func streamChunk(message map[string]any, finish any, usage bool) map[string]any {
	chunk := map[string]any{
		"status_code": 200,
		"request_id":  "req_1",
		"code":        "",
		"message":     "",
		"output": map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": finish,
				"message":       message,
			}},
		},
	}
	if usage {
		chunk["usage"] = map[string]any{
			"input_tokens":  12,
			"output_tokens": 7,
			"total_tokens":  19,
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
		ID:   "qwen",
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

func newTestRuntime(t *testing.T, server *dashServer) *inference.Runtime {
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
		{name: "base url ok", spec: `{"base_url":"https://ws.cn-beijing.maas.aliyuncs.com"}`},
		{name: "base url not http", spec: `{"base_url":"dashscope.aliyuncs.com"}`, wantErr: "http(s)"},
		{name: "model ok", spec: `{"models":[{"name":"qwen3.9","kind":"generate","vision":true,"reasoning":true}]}`},
		{name: "model bad name", spec: `{"models":[{"name":"-bad"}]}`, wantErr: "invalid model name"},
		{name: "model bad kind", spec: `{"models":[{"name":"m","kind":"bogus"}]}`, wantErr: "unsupported kind"},
		{name: "model embed kind ok", spec: `{"models":[{"name":"m","kind":"embed","vision":true}]}`},
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
	if provider.ID != "qwen" {
		t.Fatalf("provider id = %q", provider.ID)
	}
	if len(provider.Models) != len(catalog) {
		t.Fatalf("models = %d, want %d", len(provider.Models), len(catalog))
	}
	names := make(map[string]bool, len(provider.Models))
	for _, model := range provider.Models {
		names[model.Descriptor.ID.Name] = true
		switch catalog[model.Descriptor.ID.Name].kind {
		case kindGenerate:
			if model.Openers.Generate == nil {
				t.Fatalf("model %q has no generate opener", model.Descriptor.ID.Name)
			}
		case kindEmbed:
			if model.Openers.Embed == nil {
				t.Fatalf("model %q has no embed opener", model.Descriptor.ID.Name)
			}
		}
	}
	if !names["qwen-plus"] || !names["qwen3.7-plus"] || !names["qwen3-vl-plus"] ||
		!names["text-embedding-v4"] || !names["qwen3-vl-embedding"] {
		t.Fatalf("catalog names = %v", names)
	}
}

func TestFactoryRejectsBadSecrets(t *testing.T) {
	raw := []byte(`{}`)
	_, err := Factory().Build(context.Background(), config.ProviderInput{
		ID:   "qwen",
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
		ID:       "qwen",
		Spec:     raw,
		Profiles: []config.ResolvedProfile{{ID: "default"}},
	})
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err = %v", err)
	}
}
