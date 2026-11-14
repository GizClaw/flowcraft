package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/goccy/go-json"
)

// TestGenerationOptionsDecodeFromConfig walks the whole configuration path: a
// deployment entry becomes a typed extension through the driver's registry, then
// reaches the wire. This is the mechanism that lets one provider instance serve
// nodes with different settings.
func TestGenerationOptionsDecodeFromConfig(t *testing.T) {
	entries := []inference.ExtensionEntry{{
		Provider: ProviderID,
		ID:       GenerationExtensionID,
		Fields: json.RawMessage(`{
			"temperature": 0.2,
			"max_output_tokens": 512,
			"reasoning_effort": "high",
			"response_format": {"kind": "json_schema", "name": "answer", "schema": {"type": "object"}}
		}`),
	}}
	extensions, err := inference.DecodeExtensions(entries, Decoders(), "node extensions")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(extensions) != 1 {
		t.Fatalf("extensions = %d", len(extensions))
	}

	request := canonicalRequest(t)
	request.Extensions = extensions
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["temperature"] != 0.2 {
		t.Fatalf("temperature = %v", wire["temperature"])
	}
	if wire["max_completion_tokens"] != float64(512) {
		t.Fatalf("max_completion_tokens = %v", wire["max_completion_tokens"])
	}
	if wire["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", wire["reasoning_effort"])
	}
	format, _ := wire["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format = %v", wire["response_format"])
	}
}

// TestGenerationOptionsStrictDecode proves a typo cannot silently become a
// default: the registry decoder rejects unknown keys.
func TestGenerationOptionsStrictDecode(t *testing.T) {
	entries := []inference.ExtensionEntry{{
		Provider: ProviderID,
		ID:       GenerationExtensionID,
		Fields:   json.RawMessage(`{"temperatur": 0.2}`),
	}}
	if _, err := inference.DecodeExtensions(entries, Decoders(), "node extensions"); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestGenerationOptionsRejectOutOfRange(t *testing.T) {
	entries := []inference.ExtensionEntry{{
		Provider: ProviderID,
		ID:       GenerationExtensionID,
		Fields:   json.RawMessage(`{"temperature": 9}`),
	}}
	if _, err := inference.DecodeExtensions(entries, Decoders(), "node extensions"); err == nil {
		t.Fatal("out-of-range temperature accepted")
	}
}

// TestUnregisteredExtensionIsRejected pins that naming an option the build does
// not know fails instead of being ignored.
func TestUnregisteredExtensionIsRejected(t *testing.T) {
	entries := []inference.ExtensionEntry{{
		Provider: ProviderID,
		ID:       "no_such_extension",
		Fields:   json.RawMessage(`{}`),
	}}
	if _, err := inference.DecodeExtensions(entries, Decoders(), "node extensions"); err == nil {
		t.Fatal("unregistered extension accepted")
	}
}

// TestUnknownExtensionTypeIsRejected covers the other direction: a caller hand
// building extensions can pass one this driver does not implement.
// TestResponsesOnlyOptionRejectedOnChat proves a surface-specific option is
// refused rather than dropped.
// TestResponsesOnlyOptionAcceptedOnResponses is the counterpart: the same option
// is expressible on the surface that has it.
func TestResponsesOnlyOptionAcceptedOnResponses(t *testing.T) {
	limit := 3
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{MaxToolCalls: &limit}}
	d := &driver{settings: testSettings(APIResponses)}
	compiled, err := d.responsesCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["max_tool_calls"] != float64(3) {
		t.Fatalf("max_tool_calls = %v", wire["max_tool_calls"])
	}
}

// TestExtensionFieldsAreReported is the contract guarantee: every option the
// driver consumed appears in the compilation's field list, so the pipeline can
// prove nothing was dropped.
// foreignExtension addresses this driver but is not a type it implements.
type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return ProviderID }
func (foreignExtension) ExtensionID() string { return "foreign" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"x"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

// TestJSONSetMergesIntoRequestBody is the escape hatch's contract: the injected
// raw field rides alongside the typed parameters in the body that actually goes
// out, and the typed parameters survive.
func TestJSONSetMergesIntoRequestBody(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":0,"model":"gpt-5-mini","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	temperature := 0.4
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		Temperature: &temperature,
		JSONSet: map[string]json.RawMessage{
			"seed":     json.RawMessage(`42`),
			"logit_bi": json.RawMessage(`{"a":1}`),
		},
	}}
	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := provider.Generate(context.Background(), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if received["seed"] != float64(42) {
		t.Fatalf("injected field missing: %v", received["seed"])
	}
	if _, ok := received["logit_bi"].(map[string]any); !ok {
		t.Fatalf("injected object missing: %v", received["logit_bi"])
	}
	if received["temperature"] != 0.4 {
		t.Fatalf("typed field lost: %v", received["temperature"])
	}
	if received["model"] != "gpt-5-mini" {
		t.Fatalf("model lost: %v", received["model"])
	}
}

// TestJSONSetFieldsAreReported keeps the escape hatch inside the contract: each
// injected key is its own field, so a dropped one is detectable.
func TestJSONSetFieldsAreReported(t *testing.T) {
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		JSONSet: map[string]json.RawMessage{"seed": json.RawMessage(`7`)},
	}}
	d := &driver{settings: testSettings(APIChat)}
	compiled, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	want := inference.Field("extension.openai.generation.json.seed")
	for _, field := range compiled.Fields {
		if field == want {
			return
		}
	}
	t.Fatalf("injected field %q not reported: %v", want, compiled.Fields)
}

func TestWebSearchToolOnResponses(t *testing.T) {
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		WebSearch: &WebSearch{ContextSize: "high", AllowedDomains: []string{"example.com"}},
	}}
	d := &driver{settings: testSettings(APIResponses)}
	compiled, err := d.responsesCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	tools, _ := wire["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want the function tool plus web_search", len(tools))
	}
	var search map[string]any
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] == "web_search" {
			search = tool
		}
	}
	if search == nil {
		t.Fatalf("web_search tool missing: %v", tools)
	}
	if search["search_context_size"] != "high" {
		t.Fatalf("context size = %v", search["search_context_size"])
	}
	filters, _ := search["filters"].(map[string]any)
	domains, _ := filters["allowed_domains"].([]any)
	if len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("allowed domains = %v", filters["allowed_domains"])
	}
}

func TestWebSearchRejectedOnChat(t *testing.T) {
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{
		WebSearch: &WebSearch{ContextSize: "low"},
	}}
	d := &driver{settings: testSettings(APIChat)}
	if _, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}

func TestStoreOnResponses(t *testing.T) {
	store := true
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{Store: &store}}
	d := &driver{settings: testSettings(APIResponses)}
	compiled, err := d.responsesCompile(context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := wireMap(t, compiled.Wire)
	if wire["store"] != true {
		t.Fatalf("store = %v", wire["store"])
	}
}

func TestStoreRejectedOnChat(t *testing.T) {
	store := false
	request := canonicalRequest(t)
	request.Extensions = inference.Extensions{&GenerationOptions{Store: &store}}
	d := &driver{settings: testSettings(APIChat)}
	if _, err := d.chatCompile(context.Background(), request, protocol.ShapeUnary); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("kind = %v, want UnsupportedFeature", err)
	}
}
