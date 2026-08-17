package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/plugin"
	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResult struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var generateResponseJSON = json.RawMessage(`{
  "message": {
    "role": "assistant",
    "content": {"parts": [{"type": "text", "text": "hello from the echo provider"}]}
  },
  "finish_reason": "completed",
  "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}
}`)

// newProviderServer returns an httptest server implementing the
// plugin RPC protocol and, when streaming is enabled, the SSE /stream
// endpoint.
func newProviderServer(t *testing.T, streaming bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req rpcEnvelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc: %v", err)
			return
		}
		var result json.RawMessage
		var rpcErr *rpcError
		switch req.Method {
		case "plugin.handshake":
			result = mustJSON(service.Handshake{
				ProtocolVersion: service.ProtocolVersion1,
				Name:            "acme.echo",
				Capabilities: []service.Capability{{
					Kind:      "inference.Provider",
					Impl:      "echo",
					Spec:      resource.Spec{Kind: "inference.Provider", Impl: "echo"},
					Streaming: streaming,
				}},
			})
		case "resource.new":
			result = mustJSON(struct {
				Handle string `json:"handle"`
			}{Handle: "h-1"})
		case "resource.call":
			result = generateResponseJSON
		case "resource.close":
			result = json.RawMessage("{}")
		default:
			rpcErr = &rpcError{Code: -32601, Message: "method not found"}
		}
		_ = json.NewEncoder(w).Encode(rpcResult{
			JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr,
		})
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		if !streaming {
			http.Error(w, "streaming not supported", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, event := range []string{
			`{"part_index":0,"delta":{"text":"hello "}}`,
			`{"part_index":0,"delta":{"text":"world"}}`,
			`{"finish_reason":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
		}
	})
	return httptest.NewServer(mux)
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// buildE2E wires a plugin directory pointing at the fake provider
// through the loader into a built deploy result and returns the
// inference assembly.
func buildE2E(t *testing.T, streaming bool) *inference.Assembly {
	t.Helper()
	ctx := context.Background()
	server := newProviderServer(t, streaming)
	t.Cleanup(server.Close)

	root := t.TempDir()
	dir := filepath.Join(root, "acme.echo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`name: acme.echo
version: 1.0.0
artifacts:
  - type: service
    transport: http
    url: %q
    capabilities:
      - kind: inference.Provider
        impl: echo
`, server.URL)
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := plugin.NewLoader(plugin.WithServicePluginBuilder(
		func(m plugin.Manifest, s service.Spec) ([]plugin.Plugin, error) {
			p, err := NewPlugin(m, s)
			if err != nil {
				return nil, err
			}
			return []plugin.Plugin{p}, nil
		},
	))
	set, err := loader.Load(ctx, plugin.PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.echo"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	target := plugin.NewTarget()
	if err := set.Apply(ctx, target); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	reg := target.Resources
	if err := inference.Register(reg); err != nil {
		t.Fatalf("inference.Register: %v", err)
	}

	doc, err := utils.Decode[deploy.Document]([]byte(`version: "1"
resources:
  provider:
    kind: inference.Provider
    impl: echo
    settings:
      id: echo
      models: [echo-1]
  assembly:
    kind: inference.Assembly
    impl: unified
    deps:
      provider: provider
`))
	if err != nil {
		t.Fatalf("decode document: %v", err)
	}
	result, err := deploy.NewBuilder(reg).Build(ctx, doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	value, ok := result.Value("assembly")
	if !ok {
		t.Fatal("assembly missing from build result")
	}
	assembly, ok := value.(*inference.Assembly)
	if !ok {
		t.Fatalf("assembly is %T", value)
	}
	return assembly
}

func testGenerateRequest() inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "hi"}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
}

func TestProviderEndToEnd(t *testing.T) {
	ctx := context.Background()
	assembly := buildE2E(t, true)
	ref := inference.ModelRef{
		ID: inference.ModelID{Provider: "echo", Name: "echo-1"},
	}
	req := testGenerateRequest()

	resp, err := assembly.Generate(ctx, ref, req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := resp.Message.Content.Text(); got != "hello from the echo provider" {
		t.Fatalf("generate text = %q", got)
	}

	stream, err := assembly.GenerateStream(ctx, ref, req)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var text strings.Builder
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream Next: %v", err)
		}
		if delta, ok := event.Delta.(inference.TextPartDelta); ok {
			text.WriteString(delta.Text)
		}
	}
	if got := text.String(); got != "hello world" {
		t.Fatalf("stream text = %q", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}
}

func TestProviderUnaryOnlyWithoutStreaming(t *testing.T) {
	ctx := context.Background()
	assembly := buildE2E(t, false)
	ref := inference.ModelRef{
		ID: inference.ModelID{Provider: "echo", Name: "echo-1"},
	}
	req := testGenerateRequest()

	if _, err := assembly.Generate(ctx, ref, req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := assembly.GenerateStream(ctx, ref, req); err == nil {
		t.Fatal("GenerateStream must fail for a unary-only plugin")
	}
}

func TestNewPluginRejectsUnsupportedCapability(t *testing.T) {
	manifest := plugin.Manifest{
		Name:    "acme.box",
		Version: "1.0.0",
		Artifacts: []plugin.Artifact{{
			Type:      "service",
			Transport: "http",
			URL:       "http://127.0.0.1:1",
			Capabilities: []resource.Spec{{
				Kind: "sandbox.Runner",
				Impl: "rpc",
			}},
		}},
	}
	if _, err := NewPlugin(manifest, service.Spec{
		Transport: service.TransportHTTP,
		URL:       "http://127.0.0.1:1",
	}); err == nil {
		t.Fatal("NewPlugin must reject unsupported capability kinds")
	}
}

func TestProviderMissingSettingsID(t *testing.T) {
	ctx := context.Background()
	svc, err := service.New(service.Spec{
		Transport: service.TransportHTTP,
		URL:       "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := providerFactory{
		svc: svc,
		spec: resource.Spec{
			Kind: "inference.Provider",
			Impl: "echo",
		},
	}
	if _, err := factory.New(ctx, resource.Input{
		Settings: []byte(`{"models":["echo-1"]}`),
	}); err == nil {
		t.Fatal("factory must reject settings without id")
	}
}

func TestProviderHandleClose(t *testing.T) {
	var mu sync.Mutex
	var lastMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcEnvelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc: %v", err)
			return
		}
		mu.Lock()
		lastMethod = req.Method
		mu.Unlock()
		var result json.RawMessage
		switch req.Method {
		case "plugin.handshake":
			result = mustJSON(service.Handshake{
				ProtocolVersion: service.ProtocolVersion1,
				Name:            "acme.echo",
				Capabilities: []service.Capability{{
					Kind: "inference.Provider",
					Impl: "echo",
					Spec: resource.Spec{Kind: "inference.Provider", Impl: "echo"},
				}},
			})
		case "resource.new":
			result = mustJSON(struct {
				Handle string `json:"handle"`
			}{Handle: "h-1"})
		case "resource.close":
			result = json.RawMessage("{}")
		}
		_ = json.NewEncoder(w).Encode(rpcResult{
			JSONRPC: "2.0", ID: req.ID, Result: result,
		})
	}))
	defer server.Close()

	svc, err := service.New(service.Spec{
		Transport: service.TransportHTTP,
		URL:       server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := svc.New(context.Background(), "inference.Provider/echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &rpcProvider{svc: svc, handle: handle}
	if err := provider.Close(); err != nil {
		t.Fatalf("rpcProvider.Close: %v", err)
	}
	if !svc.Healthy() {
		t.Fatal("per-handle close must not tear the service down")
	}
	mu.Lock()
	got := lastMethod
	mu.Unlock()
	if got != "resource.close" {
		t.Fatalf("last RPC method = %q, want resource.close", got)
	}
}
