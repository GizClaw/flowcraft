package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// newCaptureServer returns an httptest server that decodes the request
// body into got and replies with a minimal valid Responses payload.
func newCaptureServer(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "ok"}},
			}},
		})
	}))
}

func TestBuildRequest_StoreDefaultsFalse(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if store, ok := got["store"].(bool); !ok || store {
		t.Fatalf("store = %#v, want false (default)", got["store"])
	}
}

func TestBuildRequest_StoreExtraOverride(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		llm.WithExtra("store", true),
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if store, ok := got["store"].(bool); !ok || !store {
		t.Fatalf("store = %#v, want true", got["store"])
	}
}

func TestBuildRequest_PreviousResponseID(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "more")},
		llm.WithExtra("previous_response_id", "resp_123"),
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got["previous_response_id"] != "resp_123" {
		t.Fatalf("previous_response_id = %#v", got["previous_response_id"])
	}
}

func TestBuildRequest_ThinkingAutoViaExtra(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "prove Riemann")},
		llm.WithExtra("thinking", "auto"),
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want object", got["thinking"])
	}
	if thinking["type"] != "auto" {
		t.Fatalf("thinking.type = %#v, want auto", thinking["type"])
	}
}

func TestBuildRequest_ThinkingOmittedWhenNil(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, present := got["thinking"]; present {
		t.Fatalf("thinking = %#v, want omitted when neither opts.Thinking nor Extra set", got["thinking"])
	}
}

func TestBuildRequest_ReasoningEffortViaExtra(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "think hard")},
		llm.WithExtra("thinking", "enabled"),
		llm.WithExtra("reasoning_effort", "high"),
	); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want object", got["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning.effort = %#v, want high", reasoning["effort"])
	}
}

func TestGenerate_UsesResponsesImageFileID(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartFile, File: &llm.FileRef{URI: "file_id://img_123", MimeType: "image/png", Name: "pic.png"}},
		},
	}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	input := got["input"].([]any)
	msg := input[0].(map[string]any)
	content := msg["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "input_image" {
		t.Fatalf("type = %#v", img["type"])
	}
	if img["file_id"] != "img_123" {
		t.Fatalf("file_id = %#v, want img_123", img["file_id"])
	}
	if _, hasURL := img["image_url"]; hasURL {
		t.Fatalf("image_url = %#v, want absent for file_id image", img["image_url"])
	}
}

func TestGenerate_UsesResponsesFileDataFilenameDefault(t *testing.T) {
	var got map[string]any
	srv := newCaptureServer(t, &got)
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartFile, File: &llm.FileRef{URI: "data:application/pdf;base64,JVBERi0=", MimeType: "application/pdf"}},
		},
	}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	input := got["input"].([]any)
	msg := input[0].(map[string]any)
	content := msg["content"].([]any)
	file := content[0].(map[string]any)
	if file["type"] != "input_file" {
		t.Fatalf("type = %#v", file["type"])
	}
	if file["filename"] != "file.pdf" {
		t.Fatalf("filename = %#v, want file.pdf (derived from MIME)", file["filename"])
	}
}

func TestClassifyResponseEventError_ExactCode(t *testing.T) {
	tests := []struct {
		code string
		want func(error) bool
	}{
		{"rate_limit_exceeded", errdefs.IsRateLimit},
		{"RateLimitExceeded", errdefs.IsRateLimit},
		{"authentication_error", errdefs.IsUnauthorized},
		{"forbidden", errdefs.IsForbidden},
		{"insufficient_quota_error", errdefs.IsForbidden},
		{"invalid_request_error", errdefs.IsValidation},
		{"not_found", errdefs.IsValidation},
		{"InvalidParameter", errdefs.IsValidation},
	}
	for _, tc := range tests {
		err := classifyResponseEventError("stream error", tc.code, "detail")
		if !tc.want(err) {
			t.Fatalf("code %q classified wrong: %v", tc.code, err)
		}
	}
}
