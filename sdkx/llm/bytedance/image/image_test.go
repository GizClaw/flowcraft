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

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New("doubao-seedream-5-0-260128", "", "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_TextToImage_DefaultPath(t *testing.T) {
	resp := apiResponse{
		Data: []responseItem{{URL: "https://cdn/x.png", Size: "2048x2048"}},
	}
	body, _ := json.Marshal(resp)

	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, err := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "a fox in the snow"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if captured.Model != "doubao-seedream-5-0-260128" {
		t.Errorf("model: %q", captured.Model)
	}
	if captured.Prompt != "a fox in the snow" {
		t.Errorf("prompt: %q", captured.Prompt)
	}
	if captured.Image != nil {
		t.Errorf("expected no image refs, got %+v", captured.Image)
	}
	if len(out.Parts) != 1 || out.Parts[0].Image == nil || out.Parts[0].Image.URL != "https://cdn/x.png" {
		t.Fatalf("unexpected parts: %+v", out.Parts)
	}
}

func TestGenerate_SingleReference_StringForm(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{URL: "https://cdn/out.png"}}}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-4-5-251128", "test-key", srv.URL)

	msg := model.Message{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
			{Type: model.PartText, Text: "swap outfit"},
		},
	}
	if _, _, err := l.Generate(context.Background(), []llm.Message{msg}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Single ref → string form per upstream docs.
	s, ok := captured.Image.(string)
	if !ok {
		t.Fatalf("expected string image, got %T (%+v)", captured.Image, captured.Image)
	}
	if s != "https://ref/a.png" {
		t.Errorf("image: %q", s)
	}
}

func TestGenerate_MultiReference_ArrayForm(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{URL: "https://cdn/out.png"}}}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-4-5-251128", "test-key", srv.URL)

	msg := model.Message{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/b.png"}},
			{Type: model.PartText, Text: "merge them"},
		},
	}
	if _, _, err := l.Generate(context.Background(), []llm.Message{msg}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	arr, ok := captured.Image.([]any)
	if !ok {
		t.Fatalf("expected []any image, got %T (%+v)", captured.Image, captured.Image)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(arr))
	}
}

func TestGenerate_SequentialOptionsForN(t *testing.T) {
	resp := apiResponse{
		Data: []responseItem{
			{URL: "https://cdn/1.png"},
			{URL: "https://cdn/2.png"},
			{URL: "https://cdn/3.png"},
		},
		Usage: &apiUsage{GeneratedImages: 3},
	}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)

	out, usage, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "story panels")},
		llm.WithImageGen(llm.ImageGenOptions{N: 3, Width: 2048, Height: 2048}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.SequentialImageGeneration != "auto" {
		t.Errorf("sequential_image_generation: %q", captured.SequentialImageGeneration)
	}
	if captured.SequentialImageGenerationOptions == nil || captured.SequentialImageGenerationOptions.MaxImages != 3 {
		t.Errorf("sequential options: %+v", captured.SequentialImageGenerationOptions)
	}
	if captured.Size != "2048x2048" {
		t.Errorf("size: %q", captured.Size)
	}
	if len(out.Parts) != 3 {
		t.Errorf("expected 3 parts, got %d", len(out.Parts))
	}
	if usage.OutputTokens != 3 || usage.Model == "" {
		t.Errorf("usage: %+v", usage)
	}
}

func TestGenerate_NCapEnforced(t *testing.T) {
	l, _ := New("doubao-seedream-5-0-260128", "test-key", "https://invalid")
	msgs := []llm.Message{{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/1.png"}},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/2.png"}},
			{Type: model.PartText, Text: "boom"},
		},
	}}
	_, _, err := l.Generate(context.Background(), msgs,
		llm.WithImageGen(llm.ImageGenOptions{N: 14}))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error for refs+N>15, got %v", err)
	}
}

func TestGenerate_ResponseFormatBase64(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{B64JSON: "aGVsbG8="}}}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
	out, _, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "x")},
		llm.WithImageGen(llm.ImageGenOptions{ResponseFormat: llm.ResponseFormatBase64}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.ResponseFormat != "b64_json" {
		t.Errorf("response_format: %q", captured.ResponseFormat)
	}
	if len(out.Parts) != 1 || out.Parts[0].Image.Base64 != "aGVsbG8=" {
		t.Fatalf("parts: %+v", out.Parts)
	}
}

func TestGenerate_ExtraEscapeHatches(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{URL: "https://cdn/x.png"}}}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-4-0-250828", "test-key", srv.URL)
	_, _, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "garden")},
		llm.WithExtra("size", "2K"),
		llm.WithExtra("output_format", "png"),
		llm.WithExtra("watermark", false),
		llm.WithExtra("optimize_prompt_mode", "fast"),
		llm.WithExtra("web_search", true),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.Size != "2K" {
		t.Errorf("size: %q", captured.Size)
	}
	if captured.OutputFormat != "png" {
		t.Errorf("output_format: %q", captured.OutputFormat)
	}
	if captured.Watermark == nil || *captured.Watermark != false {
		t.Errorf("watermark: %+v", captured.Watermark)
	}
	if captured.OptimizePromptOptions == nil || captured.OptimizePromptOptions.Mode != "fast" {
		t.Errorf("optimize: %+v", captured.OptimizePromptOptions)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Type != "web_search" {
		t.Errorf("tools: %+v", captured.Tools)
	}
}

func TestGenerate_ExtraSizeOverridesPixelSize(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{URL: "https://cdn/x.png"}}}
	body, _ := json.Marshal(resp)
	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
	_, _, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "x")},
		llm.WithImageGen(llm.ImageGenOptions{Width: 1024, Height: 1024}),
		llm.WithExtra("size", "4K"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.Size != "4K" {
		t.Errorf("Extra size should override W/H, got %q", captured.Size)
	}
}

func TestGenerate_EmptyPromptRejected(t *testing.T) {
	l, _ := New("doubao-seedream-5-0-260128", "test-key", "https://invalid")
	_, _, err := l.Generate(context.Background(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_HTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"401 → unauthorized", http.StatusUnauthorized, errdefs.IsUnauthorized},
		{"429 → rate limit", http.StatusTooManyRequests, errdefs.IsRateLimit},
		{"500 → not available", http.StatusInternalServerError, errdefs.IsNotAvailable},
		{"400 → validation", http.StatusBadRequest, errdefs.IsValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockServer(t, tc.status, []byte(`{"error":{"code":"X"}}`), nil)
			defer srv.Close()
			l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
			_, _, err := l.Generate(context.Background(),
				[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
			if err == nil || !tc.check(err) {
				t.Fatalf("expected categorised error, got %v", err)
			}
		})
	}
}

func TestGenerate_APIErrorEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		code  string
		check func(error) bool
	}{
		{"AuthenticationError → unauthorized", "AuthenticationError", errdefs.IsUnauthorized},
		{"RateLimitExceeded → rate limit", "RateLimitExceeded.Resource", errdefs.IsRateLimit},
		{"InsufficientBalance → forbidden", "InsufficientBalance", errdefs.IsForbidden},
		{"InvalidParameter → validation", "InvalidParameter.Prompt", errdefs.IsValidation},
		{"InternalServiceError → not available", "InternalServiceError", errdefs.IsNotAvailable},
		{"unknown → internal", "WhoKnows", errdefs.IsInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(apiResponse{Error: &apiError{Code: tc.code, Message: "boom"}})
			srv := newMockServer(t, http.StatusOK, body, nil)
			defer srv.Close()
			l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
			_, _, err := l.Generate(context.Background(),
				[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
			if err == nil || !tc.check(err) {
				t.Fatalf("expected categorised error, got %v", err)
			}
		})
	}
}

func TestGenerate_ZeroImagesIsInternal(t *testing.T) {
	body, _ := json.Marshal(apiResponse{Data: []responseItem{}})
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()
	l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
	_, _, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("expected internal error, got %v", err)
	}
}

func TestGenerate_TransportErrorIsNotAvailable(t *testing.T) {
	l, _ := New("doubao-seedream-5-0-260128", "test-key", "http://127.0.0.1:1")
	_, _, err := l.Generate(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "x")})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable, got %v", err)
	}
}

func TestGenerateStream_OneShot(t *testing.T) {
	resp := apiResponse{Data: []responseItem{{URL: "https://cdn/x.png"}}}
	body, _ := json.Marshal(resp)
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	l, _ := New("doubao-seedream-5-0-260128", "test-key", srv.URL)
	stream, err := l.GenerateStream(context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "a frog")})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if !stream.Next() {
		t.Fatal("expected first Next() to be true")
	}
	if stream.Next() {
		t.Fatal("expected second Next() to be false")
	}
	finalMsg := stream.Message()
	if len(finalMsg.Parts) != 1 || finalMsg.Parts[0].Image == nil {
		t.Fatalf("Message() did not carry image: %+v", finalMsg)
	}
}

func TestProviderRegistration(t *testing.T) {
	wantModels := []string{
		"doubao-seedream-5-0-260128",
		"doubao-seedream-5-0-lite-260128",
		"doubao-seedream-4-5-251128",
		"doubao-seedream-4-0-250828",
	}
	for _, m := range wantModels {
		if _, ok := llm.DefaultRegistry.LookupModel(providerKey, m); !ok {
			t.Errorf("model %q not registered under %q", m, providerKey)
		}
	}
	spec := llm.DefaultRegistry.LookupModelSpec(providerKey, "doubao-seedream-5-0-260128")
	if spec.Caps.Supports(llm.CapTools) {
		t.Error("expected CapTools to be disabled")
	}
	if !spec.Caps.Supports(llm.CapImageOutput) {
		t.Error("expected CapImageOutput to be ENABLED")
	}
	if spec.Caps.Supports(llm.CapAudioOutput) {
		t.Error("expected CapAudioOutput to be disabled")
	}
}
