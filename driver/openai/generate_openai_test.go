package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"

	"github.com/openai/openai-go/v3/responses"
)

// ---------------------------------------------------------------------------
// Shared fixtures.
// ---------------------------------------------------------------------------

// capturedOpenAI serves the API surface used by the drivers and records
// every request body for assertion.
type capturedOpenAI struct {
	t *testing.T

	bodies  []map[string]any
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func newCapturedOpenAI(
	t *testing.T,
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any),
) (*httptest.Server, *capturedOpenAI) {
	capture := &capturedOpenAI{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		// Restore the body so handlers can re-read multipart or binary forms.
		r.Body = io.NopCloser(bytes.NewReader(payload))
		var body map[string]any
		if len(payload) > 0 &&
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("body is not JSON: %v", err)
				return
			}
		}
		capture.bodies = append(capture.bodies, body)
		handler(w, r, body)
	}))
	return server, capture
}

func (c *capturedOpenAI) body(index int) map[string]any {
	c.t.Helper()
	if index >= len(c.bodies) {
		c.t.Fatalf("only %d captured requests", len(c.bodies))
	}
	return c.bodies[index]
}

func testClients(t *testing.T, server *httptest.Server) *clients {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(
		fmt.Sprintf(`{"base_url":%q}`, server.URL),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("test-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	return cls
}

func openaiModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "openai", Name: name},
		Profile: "default",
	}
}

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

// foreignExtension is an extension owned by another provider; every openai
// compiler must reject it truthfully.
type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return "bytedance" }
func (foreignExtension) ExtensionID() string { return "generate_options" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"thinking"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

func responsesResponseJSON(output []map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id":     "resp_1",
		"object": "response",
		"status": "completed",
		"output": output,
		"usage": map[string]any{
			"input_tokens":          12,
			"output_tokens":         7,
			"total_tokens":          19,
			"input_tokens_details":  map[string]any{"cached_tokens": 3},
			"output_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	})
	return string(payload)
}

func textOutputItem(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
}

func toolCallOutputItem() map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   "call_9",
		"name":      "lookup",
		"arguments": `{"q":"openai"}`,
		"status":    "completed",
	}
}

// sseBody renders a responses-API SSE fixture: one data line per event.
func sseBody(events ...map[string]any) string {
	body := ""
	for _, event := range events {
		payload, _ := json.Marshal(event)
		body += "data: " + string(payload) + "\n\n"
	}
	return body
}

// ---------------------------------------------------------------------------
// Spec and profile validation.
// ---------------------------------------------------------------------------

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "empty", raw: `{}`, ok: true},
		{name: "base url", raw: `{"base_url":"https://gateway.example.com/v1"}`, ok: true},
		{name: "bad base url", raw: `{"base_url":"api.openai.com"}`, ok: false},
		{name: "custom model", raw: `{"models":[{"name":"my-model","kind":"generate"}]}`, ok: true},
		{name: "unknown kind", raw: `{"models":[{"name":"m","kind":"video"}]}`, ok: false},
		{name: "realtime kind deferred", raw: `{"models":[{"name":"m","kind":"realtime"}]}`, ok: false},
		{name: "duplicate model", raw: `{"models":[{"name":"m","kind":"embed"},{"name":"m","kind":"embed"}]}`, ok: false},
		{name: "bad model name", raw: `{"models":[{"name":"m x","kind":"embed"}]}`, ok: false},
		{name: "credential shaped key", raw: `{"api_key":"sk-..."}`, ok: false},
		{name: "unknown key", raw: `{"endpoints":{}}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := error(nil)
			if tc.ok {
				if _, err = decodeSpec(context.Background(), []byte(tc.raw)); err != nil {
					t.Fatalf("decodeSpec: %v", err)
				}
				return
			}
			if _, err = decodeSpec(context.Background(), []byte(tc.raw)); err == nil {
				t.Fatal("decodeSpec succeeded, want validation error")
			}
		})
	}
}

func TestProfileMaterial(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		material, err := newProfileMaterial(context.Background(), ProfileSettings{ID: "default"}, nil)
		if err != nil {
			t.Fatalf("newProfileMaterial: %v", err)
		}
		if _, err := material.newClients(context.Background(), Spec{}); err == nil {
			t.Fatal("newClients succeeded without api_key")
		}
	})
	t.Run("unknown secret", func(t *testing.T) {
		_, err := newProfileMaterial(context.Background(), ProfileSettings{
			ID:      "default",
			Secrets: map[string]resource.Secret{"access_key": resource.LiteralSecret("x")},
		}, nil)
		if err == nil {
			t.Fatal("newProfileMaterial accepted an unknown secret")
		}
	})
	t.Run("api key", func(t *testing.T) {
		material, err := newProfileMaterial(context.Background(), ProfileSettings{
			ID:      "default",
			Secrets: map[string]resource.Secret{SecretAPIKey: resource.LiteralSecret("sk-test\n")},
		}, nil)
		if err != nil {
			t.Fatalf("newProfileMaterial: %v", err)
		}
		if value, err := material.apiKey.Resolve(context.Background(), nil); err != nil ||
			value != "sk-test\n" {
			t.Fatalf("apiKey = %q, %v; want literal with newline intact", value, err)
		}
		cls, err := material.newClients(context.Background(), Spec{})
		if err != nil {
			t.Fatalf("newClients: %v", err)
		}
		if cls == nil {
			t.Fatal("newClients returned nil")
		}
	})
}

// ---------------------------------------------------------------------------
// Factory.
// ---------------------------------------------------------------------------

func TestFactoryBuild(t *testing.T) {
	input := ResourceSettings{
		ID: "openai",
		Spec: json.RawMessage(
			`{"organization":"org-1","project":"proj-1"}`,
		),
		Profiles: []ProfileSettings{{
			ID:         "default",
			Operations: []inference.Operation{inference.OperationGenerate, inference.OperationEmbed},
			Secrets:    map[string]resource.Secret{SecretAPIKey: resource.LiteralSecret("sk-test")},
		}},
	}
	provider, err := buildProvider(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if provider.ID != "openai" {
		t.Fatalf("provider ID = %q", provider.ID)
	}
	if len(provider.Profiles) != 1 || provider.Profiles[0].ID != "default" {
		t.Fatalf("profiles = %+v", provider.Profiles)
	}
	if len(provider.Models) != len(catalog) {
		t.Fatalf("models = %d, want %d", len(provider.Models), len(catalog))
	}
	byName := make(map[string]inference.ModelImplementation, len(provider.Models))
	for _, model := range provider.Models {
		byName[model.Descriptor.ID.Name] = model
	}
	if !byName["gpt-5.6-sol"].Descriptor.Capabilities.HostedWebSearch {
		t.Fatal("gpt-5.6-sol lost its hosted web search capability")
	}
	if byName["gpt-4.1-nano"].Descriptor.Capabilities.HostedWebSearch {
		t.Fatal("gpt-4.1-nano must not claim hosted web search")
	}
}

func TestFactoryCustomModelWebSearchCapability(t *testing.T) {
	input := ResourceSettings{
		ID: "openai",
		Spec: json.RawMessage(
			`{"models":[{"name":"my-search","kind":"generate","capabilities":{"hosted_web_search":true,"outputs":["text"]}}]}`,
		),
		Profiles: []ProfileSettings{{
			ID:      "default",
			Secrets: map[string]resource.Secret{SecretAPIKey: resource.LiteralSecret("sk-test")},
		}},
	}
	provider, err := buildProvider(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var custom *inference.ModelImplementation
	for index := range provider.Models {
		if provider.Models[index].Descriptor.ID.Name == "my-search" {
			custom = &provider.Models[index]
		}
	}
	if custom == nil {
		t.Fatal("my-search missing from provider models")
	}
	if !custom.Descriptor.Capabilities.HostedWebSearch {
		t.Fatal("custom model must carry the declared hosted web search capability")
	}
}

// ---------------------------------------------------------------------------
// wireToParams conversion.
// ---------------------------------------------------------------------------

func compileTextWire(t *testing.T, request inference.GenerateRequest) generateWire {
	t.Helper()
	compiled, err := compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"])(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	return compiled.Wire
}

func TestWireToParamsMessages(t *testing.T) {
	request := simpleTextRequest("current")
	request.Context = []message.Message{
		{
			Role: message.RoleSystem,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "be terse"},
			}},
		},
		{
			Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "prior"},
			}},
		},
		{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "answer"},
				message.ToolCallPart{Call: message.ToolCall{
					ID:        "call_1",
					Name:      "lookup",
					Arguments: json.RawMessage(`{"q":"x"}`),
				}},
			}},
		},
		{
			Role: message.RoleTool,
			Content: message.Content{Parts: []message.Part{
				message.ToolResultPart{Result: message.ToolResult{
					CallID:  "call_1",
					Content: "found",
				}},
			}},
		},
	}
	params := wireToParams(compileTextWire(t, request))
	items := params.Input.OfInputItemList
	if len(items) != 6 {
		t.Fatalf("items = %d, want 6 (system, user, assistant, call, output, current)", len(items))
	}
	if items[0].OfMessage == nil || string(items[0].OfMessage.Role) != "system" {
		t.Fatalf("system item = %+v", items[0])
	}
	if items[3].OfFunctionCall == nil ||
		items[3].OfFunctionCall.CallID != "call_1" ||
		items[3].OfFunctionCall.Arguments != `{"q":"x"}` {
		t.Fatalf("tool call item = %+v", items[3])
	}
	if items[4].OfFunctionCallOutput == nil ||
		items[4].OfFunctionCallOutput.Output.OfString.Value != "found" {
		t.Fatalf("tool output item = %+v", items[4])
	}
}

func TestWireToParamsKnobs(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Input.Content.Intent.Text = &inference.TextIntent{
		Response: &inference.ResponseFormat{
			Kind:   inference.ResponseJSONSchema,
			Name:   "answer",
			Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		},
		MaxOutputTokens: intPointer(64),
		Temperature:     floatPointer(0.2),
		TopP:            floatPointer(0.9),
		ReasoningEffort: inference.ReasoningHigh,
		Tools: []message.ToolDefinition{{
			Name:        "lookup",
			Description: "find things",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice: &inference.ToolChoice{Kind: inference.ToolChoiceNamed, Name: "lookup"},
	}
	params := wireToParams(compileTextWire(t, request))
	if params.MaxOutputTokens.Value != 64 {
		t.Fatalf("max tokens = %v", params.MaxOutputTokens)
	}
	if params.Temperature.Value != 0.2 || params.TopP.Value != 0.9 {
		t.Fatalf("sampling = %v/%v", params.Temperature, params.TopP)
	}
	if string(params.Reasoning.Effort) != "high" {
		t.Fatalf("reasoning effort = %q", params.Reasoning.Effort)
	}
	if len(params.Tools) != 1 || params.Tools[0].OfFunction == nil ||
		params.Tools[0].OfFunction.Name != "lookup" {
		t.Fatalf("tools = %+v", params.Tools)
	}
	if params.Tools[0].OfFunction.Description.Value != "find things" {
		t.Fatalf("tool description = %v", params.Tools[0].OfFunction.Description)
	}
	if params.ToolChoice.OfFunctionTool == nil ||
		params.ToolChoice.OfFunctionTool.Name != "lookup" {
		t.Fatalf("tool choice = %+v", params.ToolChoice)
	}
	format := params.Text.Format
	if format.OfJSONSchema == nil {
		t.Fatalf("text format = %+v", format)
	}
}

func intPointer(value int) *int           { return &value }
func floatPointer(value float64) *float64 { return &value }

// ---------------------------------------------------------------------------
// Error classification.
// ---------------------------------------------------------------------------

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"bad request", 400, errdefs.IsValidation},
		{"unauthorized", 401, errdefs.IsUnauthorized},
		{"forbidden", 403, errdefs.IsForbidden},
		{"rate limit", 429, errdefs.IsRateLimit},
		{"server", 500, errdefs.IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"message":"boom","type":"api_error","code":"x"}}`)
			})
			defer server.Close()
			cls := testClients(t, server)
			operations, err := openGenerate(cls, catalog["gpt-5.6-sol"], openaiModel("gpt-5.6-sol").ID, "default")
			if err != nil {
				t.Fatalf("openGenerate: %v", err)
			}
			_, err = operations.Unary.Execute(
				context.Background(),
				openaiModel("gpt-5.6-sol"),
				simpleTextRequest("hi"),
			)
			if err == nil {
				t.Fatal("Execute succeeded, want classified error")
			}
			if !inference.IsKind(err, inference.ProviderFailure) {
				t.Fatalf("error = %v, want ProviderFailure", err)
			}
			if !tc.check(err) {
				t.Fatalf("classified error = %v", err)
			}
		})
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("x-request-id", "req-openai")
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"x"}}`)
	})
	defer server.Close()
	cls := testClients(t, server)
	operations, err := openGenerate(
		cls,
		catalog["gpt-5.6-sol"],
		openaiModel("gpt-5.6-sol").ID,
		"default",
	)
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	_, err = operations.Unary.Execute(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
	)
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("err = %v, want rate limit", err)
	}
	if got, ok := errdefs.RetryAfter(err); !ok || got != 4*time.Second {
		t.Fatalf("RetryAfter = %v/%v, want 4s/true", got, ok)
	}
	if got := errdefs.RetryCount(err); got < 1 {
		t.Fatalf("RetryCount = %d, want at least 1 wire attempt", got)
	}
	if got, ok := errdefs.RequestID(err); !ok || got != "req-openai" {
		t.Fatalf("RequestID = %q/%v, want req-openai/true", got, ok)
	}
}

func TestSpecRejectsNegativeHTTPRetries(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{"http_retries":-1}`)); err == nil {
		t.Fatal("decodeSpec accepted negative http_retries")
	}
}

// ---------------------------------------------------------------------------
// Reasoning traces — compile, round-trip, params.
// ---------------------------------------------------------------------------

func TestWireToParamsReasoningItem(t *testing.T) {
	request := simpleTextRequest("current")
	request.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.ReasoningPart{
				Text:      "joined summary",
				Signature: "enc-1",
				ID:        "rs_1",
			},
			message.TextPart{Text: "answer"},
		}},
	}}
	compiled, err := compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"])(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	for _, item := range compiled.Report.Decisions {
		if item.Field == inference.FieldGenerateContextReasoning &&
			item.Disposition != inference.Native {
			t.Fatalf("round-trippable reasoning must compile native: %+v", item)
		}
	}
	wire := compiled.Wire
	if len(wire.items) != 3 ||
		wire.items[0].kind != wireItemReasoning ||
		wire.items[1].kind != wireItemMessage ||
		wire.items[2].kind != wireItemMessage {
		t.Fatalf("items = %+v", wire.items)
	}
	if wire.items[0].reasoningID != "rs_1" ||
		wire.items[0].summary != "joined summary" ||
		wire.items[0].encrypted != "enc-1" {
		t.Fatalf("reasoning item = %+v", wire.items[0])
	}

	params := wireToParams(wire)
	if len(params.Include) != 1 ||
		params.Include[0] != responses.ResponseIncludableReasoningEncryptedContent {
		t.Fatalf("include = %+v", params.Include)
	}
	items := params.Input.OfInputItemList
	if items[0].OfReasoning == nil ||
		items[0].OfReasoning.ID != "rs_1" ||
		items[0].OfReasoning.EncryptedContent.Value != "enc-1" ||
		len(items[0].OfReasoning.Summary) != 1 ||
		items[0].OfReasoning.Summary[0].Text != "joined summary" {
		t.Fatalf("reasoning param = %+v", items[0].OfReasoning)
	}
}

func TestCompileReasoningDispositions(t *testing.T) {
	model := openaiModel("gpt-5.6-sol")
	compile := compileGenerate("gpt-5.6-sol", catalog["gpt-5.6-sol"])

	t.Run("reasoning without id drops with reason", func(t *testing.T) {
		request := simpleTextRequest("hi")
		request.Context = []message.Message{{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ReasoningPart{Text: "trace", Signature: "enc"},
				message.TextPart{Text: "answer"},
			}},
		}}
		compiled, err := compile(
			context.Background(), model, request, inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("compileGenerate: %v", err)
		}
		found := false
		for _, item := range compiled.Report.Decisions {
			if item.Field == inference.FieldGenerateContextReasoning {
				found = true
				if item.Disposition != inference.Dropped || item.Reason == "" {
					t.Fatalf("id-less reasoning decision = %+v", item)
				}
			}
		}
		if !found {
			t.Fatal("no decision for context reasoning")
		}
		for _, item := range compiled.Wire.items {
			if item.kind == wireItemReasoning {
				t.Fatalf("id-less reasoning must not reach the wire: %+v", item)
			}
		}
	})

	t.Run("reasoning on model without reasoning channel drops", func(t *testing.T) {
		spec, err := decodeSpec(context.Background(), []byte(
			`{"models":[{"name":"my-plain-model","kind":"generate","capabilities":{"outputs":["text"]}}]}`,
		))
		if err != nil {
			t.Fatalf("decodeSpec: %v", err)
		}
		models, err := mergedCatalog(spec)
		if err != nil {
			t.Fatalf("mergedCatalog: %v", err)
		}
		request := simpleTextRequest("hi")
		request.Context = []message.Message{{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.ReasoningPart{Text: "trace", Signature: "enc", ID: "rs_1"},
			}},
		}}
		compiled, err := compileGenerate("my-plain-model", models["my-plain-model"])(
			context.Background(),
			openaiModel("my-plain-model"),
			request,
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("compileGenerate: %v", err)
		}
		found := false
		for _, item := range compiled.Report.Decisions {
			if item.Field == inference.FieldGenerateContextReasoning {
				found = true
				if item.Disposition != inference.Dropped {
					t.Fatalf("non-reasoning model decision = %+v", item)
				}
			}
		}
		if !found {
			t.Fatal("no decision for context reasoning")
		}
	})
}
