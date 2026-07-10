package anthropic

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
	maxTokens := int64(512)
	_, _, err = c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "hi"),
	}, llm.WithThinking(true), llm.WithMaxTokens(maxTokens))
	if err == nil {
		t.Fatalf("expected error for thinking with max_tokens < budget, got none")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
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

// TestConvertImagePartAnthropic verifies that the image part converter
// accepts data: URLs, http(s) URLs, and raw base64 payloads. Previously
// only data: URLs were handled, so standard image URLs were silently dropped.
func TestConvertImagePartAnthropic(t *testing.T) {
	// data: URL
	dataURL := "data:image/png;base64,abcd"
	blk, err := convertImagePartAnthropic(&llm.MediaRef{URL: dataURL})
	if err != nil {
		t.Fatalf("data: URL: %v", err)
	}
	if blk == nil || blk.OfImage == nil || blk.OfImage.Source.OfBase64 == nil {
		t.Fatalf("data: URL did not produce base64 image block")
	}

	// https URL
	blk, err = convertImagePartAnthropic(&llm.MediaRef{URL: "https://example.com/img.png"})
	if err != nil {
		t.Fatalf("https URL: %v", err)
	}
	if blk == nil || blk.OfImage == nil || blk.OfImage.Source.OfURL == nil || blk.OfImage.Source.OfURL.URL != "https://example.com/img.png" {
		t.Fatalf("https URL did not produce URL image block")
	}

	// raw base64 + media type
	blk, err = convertImagePartAnthropic(&llm.MediaRef{Base64: "abcd", MediaType: "image/png"})
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if blk == nil || blk.OfImage == nil || blk.OfImage.Source.OfBase64 == nil || blk.OfImage.Source.OfBase64.Data != "abcd" {
		t.Fatalf("base64 did not produce base64 image block")
	}

	// empty
	blk, err = convertImagePartAnthropic(&llm.MediaRef{})
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if blk != nil {
		t.Fatalf("empty image ref should produce nil block")
	}

	// unsupported scheme
	_, err = convertImagePartAnthropic(&llm.MediaRef{URL: "s3://bucket/img.png"})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error for unsupported scheme, got %v", err)
	}
}

// TestConvertFilePartAnthropic verifies that PDF data: URIs are parsed
// into base64 document blocks and that non-PDF / non-image file types are
// rejected instead of having their URI sent as document text.
func TestConvertFilePartAnthropic(t *testing.T) {
	// PDF data: URI -> base64 document block
	pdfData := "data:application/pdf;base64,JVBERi0xLg=="
	blk, err := convertFilePartAnthropic(&llm.FileRef{URI: pdfData, MimeType: "application/pdf"})
	if err != nil {
		t.Fatalf("PDF data: URI: %v", err)
	}
	if blk == nil || blk.OfDocument == nil || blk.OfDocument.Source.OfBase64 == nil || blk.OfDocument.Source.OfBase64.Data != "JVBERi0xLg==" {
		t.Fatalf("PDF data: URI did not produce base64 document block")
	}

	// PDF https URL -> URL document block
	blk, err = convertFilePartAnthropic(&llm.FileRef{URI: "https://example.com/doc.pdf", MimeType: "application/pdf"})
	if err != nil {
		t.Fatalf("PDF https URL: %v", err)
	}
	if blk == nil || blk.OfDocument == nil || blk.OfDocument.Source.OfURL == nil || blk.OfDocument.Source.OfURL.URL != "https://example.com/doc.pdf" {
		t.Fatalf("PDF https URL did not produce URL document block")
	}

	// CSV URI must be rejected, not sent as plain text
	_, err = convertFilePartAnthropic(&llm.FileRef{URI: "https://example.com/report.csv", MimeType: "text/csv"})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error for CSV, got %v", err)
	}

	// Unsupported mime type
	_, err = convertFilePartAnthropic(&llm.FileRef{URI: "data:text/plain;base64,abcd", MimeType: "text/plain"})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error for text/plain, got %v", err)
	}
}

func thinkingCaptureServer(t *testing.T, captured chan<- map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		captured <- parsed
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_01Test",
			"type": "message",
			"role": "assistant",
			"content": [{"type":"text","text":"hi"}],
			"model": "claude-3-sonnet-20240229",
			"stop_reason": "end_turn",
			"usage": {"input_tokens":5,"output_tokens":2}
		}`)
	}))
}

func readCapturedBody(t *testing.T, captured <-chan map[string]any) map[string]any {
	t.Helper()
	body := <-captured
	if body == nil {
		t.Fatalf("no captured body")
	}
	return body
}

func assertThinking(t *testing.T, body map[string]any, wantType string, wantBudget int64) {
	t.Helper()
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking map, got %#v", body["thinking"])
	}
	if got := thinking["type"]; got != wantType {
		t.Fatalf("thinking.type = %v, want %v", got, wantType)
	}
	if wantBudget > 0 {
		if got := thinking["budget_tokens"]; got != float64(wantBudget) {
			t.Fatalf("thinking.budget_tokens = %v, want %v", got, wantBudget)
		}
	} else if _, hasBudget := thinking["budget_tokens"]; hasBudget {
		t.Fatalf("expected no budget_tokens for disabled thinking, got %#v", thinking)
	}
}
