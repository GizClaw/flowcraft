package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// ---------------------------------------------------------------------------
// Shared fixtures.
// ---------------------------------------------------------------------------

// capturedAnthropic serves the Messages API surface and records every
// request body for assertion.
type capturedAnthropic struct {
	t *testing.T

	bodies  []map[string]any
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func newCapturedAnthropic(
	t *testing.T,
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any),
) (*httptest.Server, *capturedAnthropic) {
	capture := &capturedAnthropic{t: t, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
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
		capture.bodies = append(capture.bodies, body)
		handler(w, r, body)
	}))
	return server, capture
}

func (c *capturedAnthropic) body(index int) map[string]any {
	c.t.Helper()
	if index >= len(c.bodies) {
		c.t.Fatalf("only %d captured requests", len(c.bodies))
	}
	return c.bodies[index]
}

func testClients(t *testing.T, server *httptest.Server) *clients {
	t.Helper()
	spec, err := decodeSpec([]byte(
		fmt.Sprintf(`{"base_url":%q}`, server.URL),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return profileMaterial{apiKey: "test-key"}.newClients(spec)
}

func claudeModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "anthropic", Name: name},
		Profile: "default",
	}
}

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

// foreignExtension is an extension owned by another provider; the anthropic
// compiler must reject it truthfully.
type foreignExtension struct{}

func (foreignExtension) ProviderID() string  { return "openai" }
func (foreignExtension) ExtensionID() string { return "generate_options" }
func (foreignExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"thinking"}
}
func (foreignExtension) Validate() error            { return nil }
func (foreignExtension) Clone() inference.Extension { return foreignExtension{} }

func testSecret(t *testing.T, value string) config.Secret {
	t.Helper()
	secret, err := config.NewSecret([]byte(value))
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	return secret
}

func messageJSON(content []map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id":            "msg_1",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-sonnet-5",
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                12,
			"output_tokens":               7,
			"cache_creation_input_tokens": 4,
			"cache_read_input_tokens":     3,
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

// ---------------------------------------------------------------------------
// Spec, profile, factory.
// ---------------------------------------------------------------------------

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "empty", raw: `{}`, ok: true},
		{name: "base url", raw: `{"base_url":"https://gateway.example.com"}`, ok: true},
		{name: "bad base url", raw: `{"base_url":"api.anthropic.com"}`},
		{name: "custom model", raw: `{"models":[{"name":"my-claude","vision":true}]}`, ok: true},
		{name: "duplicate model", raw: `{"models":[{"name":"m"},{"name":"m"}]}`},
		{name: "bad model name", raw: `{"models":[{"name":"m x"}]}`},
		{name: "unknown key", raw: `{"kind":"generate"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec([]byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("decodeSpec: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("decodeSpec succeeded, want validation error")
			}
		})
	}
}

func TestProfileMaterial(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		if _, err := newProfileMaterial(config.ResolvedProfile{ID: "default"}); err == nil {
			t.Fatal("newProfileMaterial succeeded without api_key")
		}
	})
	t.Run("unknown secret", func(t *testing.T) {
		_, err := newProfileMaterial(config.ResolvedProfile{
			ID:      "default",
			Secrets: map[string]config.Secret{"session_key": testSecret(t, "x")},
		})
		if err == nil {
			t.Fatal("newProfileMaterial accepted an unknown secret")
		}
	})
}

func TestFactoryBuild(t *testing.T) {
	input := config.ProviderInput{
		ID:   "anthropic",
		Spec: json.RawMessage(`{}`),
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{SecretAPIKey: testSecret(t, "sk-ant")},
		}},
	}
	provider, err := Factory().Build(context.Background(), input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(provider.Models) != len(catalog) {
		t.Fatalf("models = %d, want %d", len(provider.Models), len(catalog))
	}
	var legacy *inference.ModelImplementation
	for index := range provider.Models {
		if provider.Models[index].Descriptor.ID.Name == "claude-opus-4-1" {
			legacy = &provider.Models[index]
		}
		if provider.Models[index].Openers.Generate == nil {
			t.Fatalf("model %s has no generate opener", provider.Models[index].Descriptor.ID.Name)
		}
	}
	if legacy == nil {
		t.Fatal("claude-opus-4-1 missing from catalog")
	}
	if legacy.Descriptor.Lifecycle.Status != inference.ModelStatusDeprecated ||
		legacy.Descriptor.Lifecycle.Replacement == nil ||
		legacy.Descriptor.Lifecycle.Replacement.Name != "claude-opus-5" {
		t.Fatalf("legacy lifecycle = %+v", legacy.Descriptor.Lifecycle)
	}
}

// ---------------------------------------------------------------------------
// wireToParams conversion.
// ---------------------------------------------------------------------------

func TestWireToParamsMessages(t *testing.T) {
	request := simpleTextRequest("current")
	request.Context = []inference.Message{
		{
			Role: inference.RoleSystem,
			Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "be terse"},
			}},
		},
		{
			Role: inference.RoleUser,
			Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "prior"},
			}},
		},
		{
			Role: inference.RoleAssistant,
			Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "answer"},
				inference.ToolCallPart{Call: tool.Call{
					ID:        "toolu_1",
					Name:      "lookup",
					Arguments: json.RawMessage(`{"q":"x"}`),
				}},
			}},
		},
		{
			Role: inference.RoleTool,
			Content: inference.Content{Parts: []inference.Part{
				inference.ToolResultPart{Result: tool.Result{
					CallID:  "toolu_1",
					Content: "found",
				}},
			}},
		},
	}
	compiled, err := compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"])(
		context.Background(),
		claudeModel("claude-sonnet-5"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	wire := compiled.Wire
	if len(wire.system) != 1 || wire.system[0] != "be terse" {
		t.Fatalf("system = %v", wire.system)
	}
	// user(prior), assistant(answer+tool_use), user(tool_result+current) —
	// the tool result and the current input are adjacent user turns, so they
	// merge into one message per the API's no-consecutive-roles rule.
	if len(wire.messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(wire.messages))
	}
	if wire.messages[0].role != "user" ||
		wire.messages[1].role != "assistant" ||
		wire.messages[2].role != "user" {
		t.Fatalf("roles = %+v", wire.messages)
	}
	if len(wire.messages[1].blocks) != 2 ||
		wire.messages[1].blocks[1].kind != wireBlockToolUse {
		t.Fatalf("assistant blocks = %+v", wire.messages[1].blocks)
	}
	if len(wire.messages[2].blocks) != 2 ||
		wire.messages[2].blocks[0].kind != wireBlockToolResult ||
		wire.messages[2].blocks[1].kind != wireBlockText {
		t.Fatalf("merged user blocks = %+v", wire.messages[2].blocks)
	}

	params := wireToParams(wire, ReasoningControlEffort)
	if params.MaxTokens != DefaultMaxTokens {
		t.Fatalf("max tokens = %d", params.MaxTokens)
	}
	if len(params.System) != 1 || params.System[0].Text != "be terse" {
		t.Fatalf("system params = %+v", params.System)
	}
	if len(params.Messages) != 3 {
		t.Fatalf("param messages = %d", len(params.Messages))
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
		Tools: []tool.Definition{{
			Name:        "lookup",
			Description: "find things",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`,
			),
		}},
		ToolChoice: &inference.ToolChoice{Kind: inference.ToolChoiceRequired},
	}
	compiled, err := compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"])(
		context.Background(),
		claudeModel("claude-sonnet-5"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	params := wireToParams(compiled.Wire, ReasoningControlEffort)
	if params.MaxTokens != 64 {
		t.Fatalf("max tokens = %d", params.MaxTokens)
	}
	if params.Temperature.Value != 0.2 || params.TopP.Value != 0.9 {
		t.Fatalf("sampling = %v/%v", params.Temperature, params.TopP)
	}
	if string(params.OutputConfig.Effort) != "high" {
		t.Fatalf("effort = %q", params.OutputConfig.Effort)
	}
	if params.OutputConfig.Format.Schema["properties"] == nil {
		t.Fatalf("format schema = %+v", params.OutputConfig.Format.Schema)
	}
	if len(params.Tools) != 1 || params.Tools[0].OfTool == nil ||
		params.Tools[0].OfTool.Name != "lookup" {
		t.Fatalf("tools = %+v", params.Tools)
	}
	schema := params.Tools[0].OfTool.InputSchema
	if len(schema.Required) != 1 || schema.Required[0] != "q" {
		t.Fatalf("tool schema = %+v", schema)
	}
	if params.ToolChoice.OfAny == nil {
		t.Fatalf("tool choice = %+v", params.ToolChoice)
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
		{"overloaded", 529, errdefs.IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
			})
			defer server.Close()
			cls := testClients(t, server)
			operations, err := openGenerate(cls, catalog["claude-sonnet-5"], claudeModel("claude-sonnet-5").ID, "default")
			if err != nil {
				t.Fatalf("openGenerate: %v", err)
			}
			_, err = operations.Unary.Execute(
				context.Background(),
				claudeModel("claude-sonnet-5"),
				simpleTextRequest("hi"),
			)
			if err == nil {
				t.Fatal("Execute succeeded, want classified error")
			}
			if !tc.check(err) {
				t.Fatalf("classified error = %v", err)
			}
		})
	}
}

var _ = strings.TrimSpace

// ---------------------------------------------------------------------------
// Reasoning traces — compile, round-trip, decode.
// ---------------------------------------------------------------------------

func TestWireToParamsThinkingBlocks(t *testing.T) {
	request := simpleTextRequest("current")
	request.Context = []inference.Message{{
		Role: inference.RoleAssistant,
		Content: inference.Content{Parts: []inference.Part{
			inference.TextPart{Text: "answer"},
			inference.ReasoningPart{Text: "trace", Signature: "sig-1"},
			inference.ReasoningPart{Signature: "redacted-1"},
			inference.ToolCallPart{Call: tool.Call{
				ID:        "toolu_1",
				Name:      "lookup",
				Arguments: json.RawMessage(`{"q":"x"}`),
			}},
		}},
	}}
	compiled, err := compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"])(
		context.Background(),
		claudeModel("claude-sonnet-5"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	if decision := compiled.Report.Decisions; decision == nil {
		t.Fatal("report decisions missing")
	}
	for _, item := range compiled.Report.Decisions {
		if item.Field == inference.FieldGenerateContextReasoning &&
			item.Disposition != inference.Native {
			t.Fatalf("signed reasoning must compile native: %+v", item)
		}
	}

	wire := compiled.Wire
	if len(wire.messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(wire.messages))
	}
	blocks := wire.messages[0].blocks
	// Thinking and redacted blocks hoist ahead of text and tool_use,
	// preserving their relative order — the API requires them first.
	wantOrder := []wireBlockKind{
		wireBlockThinking,
		wireBlockRedactedThinking,
		wireBlockText,
		wireBlockToolUse,
	}
	if len(blocks) != len(wantOrder) {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i, want := range wantOrder {
		if blocks[i].kind != want {
			t.Fatalf("block %d kind = %q, want %q", i, blocks[i].kind, want)
		}
	}
	if blocks[0].text != "trace" || blocks[0].signature != "sig-1" {
		t.Fatalf("thinking block = %+v", blocks[0])
	}
	if blocks[1].signature != "redacted-1" || blocks[1].text != "" {
		t.Fatalf("redacted block = %+v", blocks[1])
	}

	params := wireToParams(wire, ReasoningControlEffort)
	assistant := params.Messages[0]
	if len(assistant.Content) != 4 {
		t.Fatalf("param blocks = %d", len(assistant.Content))
	}
	if assistant.Content[0].OfThinking == nil ||
		assistant.Content[0].OfThinking.Signature != "sig-1" ||
		assistant.Content[0].OfThinking.Thinking != "trace" {
		t.Fatalf("thinking param = %+v", assistant.Content[0].OfThinking)
	}
	if assistant.Content[1].OfRedactedThinking == nil ||
		assistant.Content[1].OfRedactedThinking.Data != "redacted-1" {
		t.Fatalf("redacted param = %+v", assistant.Content[1].OfRedactedThinking)
	}
}

func TestCompileReasoningDispositions(t *testing.T) {
	model := claudeModel("claude-sonnet-5")
	compile := compileGenerate("claude-sonnet-5", catalog["claude-sonnet-5"])

	t.Run("unsigned reasoning drops with reason", func(t *testing.T) {
		request := simpleTextRequest("hi")
		request.Context = []inference.Message{{
			Role: inference.RoleAssistant,
			Content: inference.Content{Parts: []inference.Part{
				inference.ReasoningPart{Text: "unsigned"},
				inference.TextPart{Text: "answer"},
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
					t.Fatalf("unsigned reasoning decision = %+v", item)
				}
			}
		}
		if !found {
			t.Fatal("no decision for context reasoning")
		}
		for _, message := range compiled.Wire.messages {
			for _, block := range message.blocks {
				if block.thinking() {
					t.Fatalf("unsigned reasoning must not reach the wire: %+v", block)
				}
			}
		}
	})

	t.Run("input reasoning rejects", func(t *testing.T) {
		request := simpleTextRequest("hi")
		request.Input.Content.Parts = append(
			request.Input.Content.Parts,
			inference.ReasoningPart{Text: "trace", Signature: "sig"},
		)
		_, err := compile(
			context.Background(), model, request, inference.GenerateExecutionUnary,
		)
		if err == nil || !inference.IsKind(err, inference.UnsupportedFeature) {
			t.Fatalf("compile error = %v, want UnsupportedFeature", err)
		}
	})
}

func TestGenerateUnaryThinkingBlocks(t *testing.T) {
	server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, messageJSON([]map[string]any{
			{"type": "thinking", "thinking": "let me think", "signature": "sig-1"},
			{"type": "redacted_thinking", "data": "opaque-9"},
			{"type": "text", "text": "answer"},
		}))
	})
	defer server.Close()
	unary, _ := instrumentedGenerateDrivers(t, server, &inferencetest.Counter{})

	response, err := unary.Execute(
		context.Background(),
		claudeModel("claude-sonnet-5"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 3 {
		t.Fatalf("parts = %+v", parts)
	}
	visible, ok := parts[0].(inference.ReasoningPart)
	if !ok || visible.Text != "let me think" || visible.Signature != "sig-1" {
		t.Fatalf("thinking part = %#v", parts[0])
	}
	redacted, ok := parts[1].(inference.ReasoningPart)
	if !ok || redacted.Text != "" || redacted.Signature != "opaque-9" {
		t.Fatalf("redacted part = %#v", parts[1])
	}
	if text, ok := parts[2].(inference.TextPart); !ok || text.Text != "answer" {
		t.Fatalf("text part = %#v", parts[2])
	}
}

func TestGenerateStreamThinkingBlocks(t *testing.T) {
	server, _ := newCapturedAnthropic(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody(
			map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": "msg_1", "type": "message", "role": "assistant",
					"model": "claude-sonnet-5", "content": []any{},
					"usage": map[string]any{
						"input_tokens": 1, "output_tokens": 0,
						"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
					},
				},
			},
			map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			},
			map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "thinking_delta", "thinking": "let me "},
			},
			map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "thinking_delta", "thinking": "consider"},
			},
			map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "signature_delta", "signature": "sig-abc"},
			},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{
				"type": "content_block_start", "index": 1,
				"content_block": map[string]any{
					"type": "redacted_thinking", "data": "opaque-1",
				},
			},
			map[string]any{"type": "content_block_stop", "index": 1},
			map[string]any{
				"type": "content_block_start", "index": 2,
				"content_block": map[string]any{"type": "text", "text": ""},
			},
			map[string]any{
				"type": "content_block_delta", "index": 2,
				"delta": map[string]any{"type": "text_delta", "text": "done"},
			},
			map[string]any{"type": "content_block_stop", "index": 2},
			map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 3},
			},
			map[string]any{"type": "message_stop"},
		))
	})
	defer server.Close()
	_, stream := instrumentedGenerateDrivers(t, server, &inferencetest.Counter{})

	generateStream, err := stream.Stream(
		context.Background(),
		claudeModel("claude-sonnet-5"),
		simpleTextRequest("hi"),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		_, err = generateStream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := generateStream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 3 {
		t.Fatalf("parts = %+v", parts)
	}
	visible, ok := parts[0].(inference.ReasoningPart)
	if !ok || visible.Text != "let me consider" || visible.Signature != "sig-abc" {
		t.Fatalf("streamed thinking = %#v", parts[0])
	}
	redacted, ok := parts[1].(inference.ReasoningPart)
	if !ok || redacted.Text != "" || redacted.Signature != "opaque-1" {
		t.Fatalf("streamed redacted = %#v", parts[1])
	}
	if text, ok := parts[2].(inference.TextPart); !ok || text.Text != "done" {
		t.Fatalf("text part = %#v", parts[2])
	}
}
