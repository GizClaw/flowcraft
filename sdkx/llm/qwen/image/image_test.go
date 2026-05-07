package image

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func newMockServer(t *testing.T, status int, body []byte, capture *apiRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != imageGenPath {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing/incorrect Authorization header: %q", got)
		}
		if capture != nil {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := json.Unmarshal(raw, capture); err != nil {
				t.Fatalf("decode body: %v (raw=%s)", err, raw)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func successBody(images ...string) []byte {
	content := make([]apiOutContentItem, 0, len(images))
	for _, u := range images {
		content = append(content, apiOutContentItem{Image: u})
	}
	resp := apiResponse{
		RequestID: "req-1",
		Output: &apiOutput{Choices: []apiChoice{{
			FinishReason: "stop",
			Message:      &apiOutMessage{Role: "assistant", Content: content},
		}}},
		Usage: &apiUsage{ImageCount: len(images), Width: 2048, Height: 2048},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New("qwen-image-2.0-pro", "", "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNew_DefaultModelAndBaseURL(t *testing.T) {
	l, err := New("", "k", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.model != defaultModel {
		t.Errorf("default model = %q, want %q", l.model, defaultModel)
	}
	if l.baseURL != defaultBaseURL {
		t.Errorf("default base URL = %q, want %q", l.baseURL, defaultBaseURL)
	}
}

func TestGenerate_TextToImage_DefaultPath(t *testing.T) {
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), &captured)
	defer srv.Close()

	l, err := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, usage, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "a kitten"}}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Role != model.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].Type != model.PartImage || msg.Parts[0].Image == nil || msg.Parts[0].Image.URL != "https://cdn/x.png" {
		t.Errorf("unexpected parts: %+v", msg.Parts)
	}
	if usage.OutputTokens != 1 {
		t.Errorf("usage.OutputTokens = %d, want 1 (mapped from image_count)", usage.OutputTokens)
	}
	if captured.Model != "qwen-image-2.0-pro" {
		t.Errorf("captured model = %q", captured.Model)
	}
	if len(captured.Input.Messages) != 1 || len(captured.Input.Messages[0].Content) != 1 || captured.Input.Messages[0].Content[0].Text != "a kitten" {
		t.Errorf("unexpected request body: %+v", captured)
	}
}

func TestGenerate_ImageGenOptionsMapping(t *testing.T) {
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png", "https://cdn/y.png"), &captured)
	defer srv.Close()

	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	seed := int64(42)
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "two cats"}}},
	}, llm.WithImageGen(llm.ImageGenOptions{Width: 2048, Height: 2048, N: 2, Seed: &seed}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Note the "*" separator (Qwen uses "W*H", not "WxH").
	if got := captured.Parameters.Size; got != "2048*2048" {
		t.Errorf("size = %q, want 2048*2048", got)
	}
	if got := captured.Parameters.N; got != 2 {
		t.Errorf("n = %d, want 2", got)
	}
	if captured.Parameters.Seed == nil || *captured.Parameters.Seed != 42 {
		t.Errorf("seed = %v, want 42", captured.Parameters.Seed)
	}
}

func TestGenerate_ExtraOverrides(t *testing.T) {
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), &captured)
	defer srv.Close()

	l, _ := New("qwen-image-max", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hello"}}},
	},
		// W/H first; Extra "size" should override.
		llm.WithImageGen(llm.ImageGenOptions{Width: 2048, Height: 2048}),
		llm.WithExtra("size", "1664*928"),
		llm.WithExtra("negative_prompt", "low quality"),
		llm.WithExtra("prompt_extend", false),
		llm.WithExtra("watermark", true),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.Parameters.Size != "1664*928" {
		t.Errorf("size = %q, want 1664*928 (extra override)", captured.Parameters.Size)
	}
	if captured.Parameters.NegativePrompt != "low quality" {
		t.Errorf("negative_prompt = %q", captured.Parameters.NegativePrompt)
	}
	if captured.Parameters.PromptExtend == nil || *captured.Parameters.PromptExtend != false {
		t.Errorf("prompt_extend = %v, want false", captured.Parameters.PromptExtend)
	}
	if captured.Parameters.Watermark == nil || *captured.Parameters.Watermark != true {
		t.Errorf("watermark = %v, want true", captured.Parameters.Watermark)
	}
}

func TestGenerate_RejectsImageReferences(t *testing.T) {
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), nil)
	defer srv.Close()

	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartText, Text: "edit this"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://cdn/in.png"}},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error for image-reference input, got %v", err)
	}
}

func TestGenerate_EmptyPrompt(t *testing.T) {
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), nil)
	defer srv.Close()

	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "   "}}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_HTTPErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"401", http.StatusUnauthorized, errdefs.IsUnauthorized},
		{"429", http.StatusTooManyRequests, errdefs.IsRateLimit},
		{"500", http.StatusInternalServerError, errdefs.IsNotAvailable},
		{"400 (no envelope)", http.StatusBadRequest, errdefs.IsValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockServer(t, tc.status, []byte(`oops`), nil)
			defer srv.Close()
			l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
			_, _, err := l.Generate(context.Background(), []llm.Message{
				{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
			})
			if err == nil || !tc.check(err) {
				t.Fatalf("status %d: expected matching errdef, got %v", tc.status, err)
			}
		})
	}
}

func TestGenerate_APIErrorEnvelope(t *testing.T) {
	tests := []struct {
		code  string
		check func(error) bool
	}{
		{"InvalidApiKey", errdefs.IsUnauthorized},
		{"Throttling.User", errdefs.IsRateLimit},
		{"AccessDenied.Unpurchased", errdefs.IsForbidden},
		{"InvalidParameter", errdefs.IsValidation},
		{"DataInspectionFailed", errdefs.IsValidation},
		{"InternalError.Algo", errdefs.IsNotAvailable},
		{"SomethingElse", errdefs.IsInternal},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			// 200 + top-level code is the typical Qwen-Image business
			// failure shape.
			body, _ := json.Marshal(apiResponse{Code: tc.code, Message: "boom", RequestID: "req-x"})
			srv := newMockServer(t, http.StatusOK, body, nil)
			defer srv.Close()
			l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
			_, _, err := l.Generate(context.Background(), []llm.Message{
				{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
			})
			if err == nil || !tc.check(err) {
				t.Fatalf("code %s: expected matching errdef, got %v", tc.code, err)
			}
		})
	}
}

func TestGenerate_400WithEnvelope(t *testing.T) {
	body := []byte(`{"code":"InvalidParameter","message":"num_images_per_prompt must be 1","request_id":"req-x"}`)
	srv := newMockServer(t, http.StatusBadRequest, body, nil)
	defer srv.Close()
	l, _ := New("qwen-image-max", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_ZeroImages(t *testing.T) {
	body, _ := json.Marshal(apiResponse{Output: &apiOutput{Choices: []apiChoice{{Message: &apiOutMessage{}}}}})
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()
	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	})
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("expected internal error for zero images, got %v", err)
	}
}

func TestGenerate_TransportError(t *testing.T) {
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), nil)
	srv.Close() // induce a transport error
	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected not-available error, got %v", err)
	}
}

func TestGenerateStream_OneShot(t *testing.T) {
	srv := newMockServer(t, http.StatusOK, successBody("https://cdn/x.png"), nil)
	defer srv.Close()
	l, _ := New("qwen-image-2.0-pro", "test-key", srv.URL, WithHTTPClient(srv.Client()))
	stream, err := l.GenerateStream(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	// One chunk; iterator-style API.
	if !stream.Next() {
		t.Fatalf("expected first Next() = true, err=%v", stream.Err())
	}
	if stream.Next() {
		t.Fatalf("expected second Next() = false")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err after exhaustion: %v", err)
	}
	final := stream.Message()
	if len(final.Parts) != 1 || final.Parts[0].Type != model.PartImage {
		t.Errorf("unexpected message after stream: %+v", final)
	}
}

func TestExtractPrompt(t *testing.T) {
	prompt, refs := extractPrompt([]llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: "ignored"}}},
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartText, Text: "first"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://cdn/in.png"}},
		}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "second"}}},
	})
	if prompt != "first\nsecond" {
		t.Errorf("prompt = %q, want %q", prompt, "first\nsecond")
	}
	if refs != 1 {
		t.Errorf("refs = %d, want 1", refs)
	}
}

func TestProviderRegistration(t *testing.T) {
	// All catalog entries must be retrievable.
	models := []string{
		"qwen-image-2.0-pro",
		"qwen-image-2.0-pro-2026-04-22",
		"qwen-image-2.0-pro-2026-03-03",
		"qwen-image-2.0",
		"qwen-image-2.0-2026-03-03",
		"qwen-image-max",
		"qwen-image-max-2025-12-30",
		"qwen-image-plus",
		"qwen-image-plus-2026-01-09",
		"qwen-image",
	}
	for _, m := range models {
		if _, ok := llm.DefaultRegistry.LookupModel(providerKey, m); !ok {
			t.Errorf("model %q not registered under provider %q", m, providerKey)
			continue
		}
		spec := llm.DefaultRegistry.LookupModelSpec(providerKey, m)
		if !spec.Caps.Supports(llm.CapImageOutput) {
			t.Errorf("model %q should support CapImageOutput", m)
		}
		if spec.Caps.Supports(llm.CapTools) {
			t.Errorf("model %q should NOT support CapTools", m)
		}
		if spec.Caps.Supports(llm.CapAudioOutput) {
			t.Errorf("model %q should NOT support CapAudioOutput", m)
		}
	}
}
