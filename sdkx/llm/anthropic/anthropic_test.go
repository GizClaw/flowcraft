package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestGenerate_ThinkingFalseWritesDisabledRequestBody(t *testing.T) {
	captured := make(chan map[string]any, 1)
	srv := thinkingCaptureServer(t, captured)
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	}, llm.WithThinking(false))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := readCapturedBody(t, captured)
	assertThinking(t, body, "disabled", 0)
}

func TestGenerate_JSONModeThinkingFalseWritesBetaDisabledRequestBody(t *testing.T) {
	captured := make(chan map[string]any, 1)
	srv := thinkingCaptureServer(t, captured)
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	}, llm.WithJSONMode(true), llm.WithThinking(false))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := readCapturedBody(t, captured)
	if _, ok := body["output_format"]; !ok {
		t.Fatalf("expected beta JSON-mode request body to include output_format, got %#v", body)
	}
	assertThinking(t, body, "disabled", 0)
}

func TestGenerate_ThinkingTrueWritesDefaultBudget(t *testing.T) {
	captured := make(chan map[string]any, 1)
	srv := thinkingCaptureServer(t, captured)
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	}, llm.WithThinking(true))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := readCapturedBody(t, captured)
	assertThinking(t, body, "enabled", defaultThinkingBudgetTokens)
}

func TestGenerate_ThinkingTrueRejectsTooSmallMaxTokens(t *testing.T) {
	captured := make(chan map[string]any, 1)
	srv := thinkingCaptureServer(t, captured)
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	}, llm.WithMaxTokens(defaultThinkingBudgetTokens), llm.WithThinking(true))
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	select {
	case body := <-captured:
		t.Fatalf("request should not have reached server, got body %#v", body)
	default:
	}
}

func thinkingCaptureServer(t *testing.T, captured chan<- map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case captured <- body:
		default:
			t.Errorf("unexpected additional request body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-sonnet-20240229",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`)
	}))
}

func readCapturedBody(t *testing.T, captured <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case body := <-captured:
		return body
	default:
		t.Fatal("server did not capture request body")
		return nil
	}
}

func assertThinking(t *testing.T, body map[string]any, wantType string, wantBudget int64) {
	t.Helper()
	raw, ok := body["thinking"]
	if !ok {
		t.Fatalf("request body missing thinking: %#v", body)
	}
	thinking, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("thinking has unexpected shape: %#v", raw)
	}
	if got := thinking["type"]; got != wantType {
		t.Fatalf("thinking.type = %v, want %q (thinking=%#v)", got, wantType, thinking)
	}
	if wantBudget == 0 {
		if _, ok := thinking["budget_tokens"]; ok {
			t.Fatalf("disabled thinking should not include budget_tokens: %#v", thinking)
		}
		return
	}
	gotBudget, ok := thinking["budget_tokens"].(float64)
	if !ok {
		t.Fatalf("thinking.budget_tokens missing or non-numeric: %#v", thinking)
	}
	if int64(gotBudget) != wantBudget {
		t.Fatalf("thinking.budget_tokens = %v, want %d", gotBudget, wantBudget)
	}
}

// TestGenerate_NilResp_NoPanic regresses the same family of bug
// fixed in sdkx/llm/openai: anthropic-sdk-go's MessageService.New
// returns (*Message, error) and the pointer can be nil if the server
// answers with literal JSON null. Without the resp==nil guard, the
// next deref of resp.Content / resp.Usage would crash the goroutine.
//
// Triggered in production by MiniMax's /anthropic-compatible
// endpoint during degraded operation; the openai-go variant of the
// same bug crashed the LongMemEval _s eval at ~9% ingest.
func TestGenerate_NilResp_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "null")
	}))
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Generate panicked on nil resp: %v", r)
		}
	}()

	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable kind, got %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error message should mention nil, got %q", err.Error())
	}
}

func TestGenerate_ThinkingFalseDisablesThinkingOnWire(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, err := New("claude-3-sonnet-20240229", "test-key", ts.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, err = c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		llm.WithThinking(false),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	if rb.Thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want type=disabled; body=%s", rb.Thinking, cap.body)
	}
}

// TestGenerateStream_TransportError verifies the streaming path
// doesn't panic on a degraded server. The anthropic-sdk-go
// NewStreaming always allocates the stream struct so a literal
// nil handle is hard to provoke through the public API, but the
// transport-error path that flows through stream.Err is the most
// common real failure and must surface a clean error, not a panic.
func TestGenerateStream_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateStream panicked: %v", r)
		}
	}()

	stream, _ := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if stream == nil {
		return
	}
	// If a stream handle was returned, draining it must not panic;
	// any surfaced error is acceptable.
	for stream.Next() {
	}
	_ = stream.Err()
	_ = stream.Close()
}

func TestConvertContentParts_DataPartUsesAnthropicTextBlock(t *testing.T) {
	blocks, err := convertContentParts([]llm.Part{{
		Type: llm.PartData,
		Data: &llm.DataRef{
			MimeType: "application/vnd.flowcraft.snapshot+json",
			Value:    map[string]any{"k": "v"},
		},
	}})
	if err != nil {
		t.Fatalf("convertContentParts: %v", err)
	}
	if len(blocks) != 1 || blocks[0].OfText == nil {
		t.Fatalf("expected one text block, got %#v", blocks)
	}

	text := blocks[0].OfText.Text
	if !strings.Contains(text, "Claude input data") {
		t.Fatalf("data block missing Claude label: %q", text)
	}
	if !strings.Contains(text, "MIME type: application/vnd.flowcraft.snapshot+json") {
		t.Fatalf("data block missing mime_type: %q", text)
	}
	if !strings.Contains(text, "JSON:\n{\"k\":\"v\"}") {
		t.Fatalf("data block missing JSON content: %q", text)
	}
}

func TestConvertContentParts_DataPartDefaultsMimeType(t *testing.T) {
	blocks, err := convertContentParts([]llm.Part{{
		Type: llm.PartData,
		Data: &llm.DataRef{Value: map[string]any{"ok": true}},
	}})
	if err != nil {
		t.Fatalf("convertContentParts: %v", err)
	}
	if len(blocks) != 1 || blocks[0].OfText == nil {
		t.Fatalf("expected one text block, got %#v", blocks)
	}
	if !strings.Contains(blocks[0].OfText.Text, "MIME type: application/json") {
		t.Fatalf("empty mime_type should default to application/json: %q", blocks[0].OfText.Text)
	}
}

func TestConvertContentParts_DataPartKeepsAdjacentTextBoundaries(t *testing.T) {
	blocks, err := convertContentParts([]llm.Part{
		{Type: llm.PartText, Text: "before"},
		{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"n": float64(1)}}},
		{Type: llm.PartText, Text: "after"},
	})
	if err != nil {
		t.Fatalf("convertContentParts: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %#v", len(blocks), blocks)
	}
	if blocks[0].OfText == nil || blocks[0].OfText.Text != "before" {
		t.Fatalf("first text block changed: %#v", blocks[0])
	}
	if blocks[2].OfText == nil || blocks[2].OfText.Text != "after" {
		t.Fatalf("last text block changed: %#v", blocks[2])
	}
	if blocks[1].OfText == nil {
		t.Fatalf("data block should be text, got %#v", blocks[1])
	}
	text := blocks[1].OfText.Text
	if strings.Contains(text, "before") || strings.Contains(text, "after") {
		t.Fatalf("data block should stay in its own Anthropic text block: %#v", blocks)
	}
	if !strings.Contains(text, "Claude input data") {
		t.Fatalf("data block missing Claude label: %q", text)
	}
}

func TestConvertMessages_SystemPartDataValidation(t *testing.T) {
	_, _, err := convertMessages([]llm.Message{{
		Role: llm.RoleSystem,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "rules"},
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"k": "v"}}},
		},
	}})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "system message") {
		t.Fatalf("error should mention system message, got %q", err.Error())
	}
}

func TestGenerate_DataPartMarshalErrorIsValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Errorf("request should not be sent after data part validation fails")
	}))
	defer srv.Close()

	c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{Value: map[string]any{"bad": math.NaN()}},
		}},
	}}

	_, _, err = c.Generate(context.Background(), msgs)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Generate error = %v, want Validation", err)
	}
	_, err = c.GenerateStream(context.Background(), msgs)
	if !errdefs.IsValidation(err) {
		t.Fatalf("GenerateStream error = %v, want Validation", err)
	}
}
