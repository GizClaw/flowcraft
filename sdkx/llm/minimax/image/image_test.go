package image

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// newMockServer returns a httptest server that decodes the inbound
// request, hands it to handler for inspection, and writes back the
// supplied raw response body with the supplied status code.
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
				t.Fatalf("decode body: %v", err)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New("image-01", "", "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_TextToImage_URL(t *testing.T) {
	resp := apiResponse{
		ID:   "img-1",
		Data: responseData{ImageURLs: []string{"https://cdn/x.png", "https://cdn/y.png"}},
	}
	body, _ := json.Marshal(resp)

	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, err := New("image-01", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "a cat in space"),
	}, llm.WithImageGen(llm.ImageGenOptions{
		AspectRatio:    "16:9",
		N:              2,
		ResponseFormat: llm.ResponseFormatURL,
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if captured.Model != "image-01" {
		t.Errorf("model: %q", captured.Model)
	}
	if captured.Prompt != "a cat in space" {
		t.Errorf("prompt: %q", captured.Prompt)
	}
	if captured.AspectRatio != "16:9" || captured.N != 2 || captured.ResponseFormat != "url" {
		t.Errorf("opts not forwarded: %+v", captured)
	}
	if len(captured.SubjectReference) != 0 {
		t.Errorf("expected no subject_reference for t2i, got %d", len(captured.SubjectReference))
	}

	if len(out.Parts) != 2 {
		t.Fatalf("expected 2 image parts, got %d", len(out.Parts))
	}
	for i, p := range out.Parts {
		if p.Type != model.PartImage || p.Image == nil {
			t.Fatalf("part %d not an image: %+v", i, p)
		}
		if p.Image.URL == "" {
			t.Errorf("part %d missing URL", i)
		}
	}
}

func TestGenerate_ImageToImage_PassesReferences(t *testing.T) {
	resp := apiResponse{Data: responseData{ImageURLs: []string{"https://cdn/out.png"}}}
	body, _ := json.Marshal(resp)

	var captured apiRequest
	srv := newMockServer(t, http.StatusOK, body, &captured)
	defer srv.Close()

	l, _ := New("image-01", "test-key", srv.URL)

	msg := model.Message{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/b.png"}},
			{Type: model.PartText, Text: "make it cyberpunk"},
		},
	}
	if _, _, err := l.Generate(context.Background(), []llm.Message{msg}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got := len(captured.SubjectReference); got != 2 {
		t.Fatalf("expected 2 subject_reference entries, got %d", got)
	}
	for i, ref := range captured.SubjectReference {
		if ref.Type != "character" {
			t.Errorf("ref[%d].type = %q", i, ref.Type)
		}
	}
	if captured.Prompt != "make it cyberpunk" {
		t.Errorf("prompt: %q", captured.Prompt)
	}
}

func TestGenerate_Base64Response(t *testing.T) {
	resp := apiResponse{Data: responseData{ImageBase64s: []string{"aGVsbG8="}}}
	body, _ := json.Marshal(resp)
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	l, _ := New("image-01", "test-key", srv.URL)
	out, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "x"),
	}, llm.WithImageGen(llm.ImageGenOptions{ResponseFormat: llm.ResponseFormatBase64}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(out.Parts) != 1 || out.Parts[0].Image.Base64 != "aGVsbG8=" {
		t.Fatalf("unexpected parts: %+v", out.Parts)
	}
}

func TestGenerate_EmptyPromptRejected(t *testing.T) {
	l, _ := New("image-01", "test-key", "https://invalid")
	_, _, err := l.Generate(context.Background(), []llm.Message{
		// no text parts at all
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
		}},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_PromptTooLong(t *testing.T) {
	l, _ := New("image-01", "test-key", "https://invalid")
	long := strings.Repeat("a", maxPromptChars+1)
	_, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, long),
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
			srv := newMockServer(t, tc.status, []byte(`{"error":"boom"}`), nil)
			defer srv.Close()
			l, _ := New("image-01", "test-key", srv.URL)
			_, _, err := l.Generate(context.Background(), []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "hi"),
			})
			if err == nil || !tc.check(err) {
				t.Fatalf("expected categorised error, got %v", err)
			}
		})
	}
}

func TestGenerate_BaseRespErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		check func(error) bool
	}{
		{"1004 → rate limit", 1004, errdefs.IsRateLimit},
		{"1008 → forbidden (insufficient balance)", 1008, errdefs.IsForbidden},
		{"1003 → unauthorized", 1003, errdefs.IsUnauthorized},
		{"2013 → validation", 2013, errdefs.IsValidation},
		{"9999 → internal", 9999, errdefs.IsInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(apiResponse{
				BaseResp: baseResp{StatusCode: tc.code, StatusMsg: "err"},
			})
			srv := newMockServer(t, http.StatusOK, body, nil)
			defer srv.Close()
			l, _ := New("image-01", "test-key", srv.URL)
			_, _, err := l.Generate(context.Background(), []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "hi"),
			})
			if err == nil || !tc.check(err) {
				t.Fatalf("expected categorised error, got %v", err)
			}
		})
	}
}

func TestGenerate_ZeroImagesIsInternal(t *testing.T) {
	body, _ := json.Marshal(apiResponse{Data: responseData{}})
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()
	l, _ := New("image-01", "test-key", srv.URL)
	_, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "x"),
	})
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("expected internal error, got %v", err)
	}
}

func TestGenerate_TransportErrorIsNotAvailable(t *testing.T) {
	l, _ := New("image-01", "test-key", "http://127.0.0.1:1") // closed port
	_, _, err := l.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "x"),
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable, got %v", err)
	}
}

func TestGenerateStream_OneShot(t *testing.T) {
	resp := apiResponse{Data: responseData{ImageURLs: []string{"https://cdn/x.png"}}}
	body, _ := json.Marshal(resp)
	srv := newMockServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	l, _ := New("image-01", "test-key", srv.URL)
	stream, err := l.GenerateStream(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "a frog"),
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if !stream.Next() {
		t.Fatal("expected first Next() to be true")
	}
	if stream.Next() {
		t.Fatal("expected second Next() to be false")
	}
	if stream.Err() != nil {
		t.Fatalf("Err: %v", stream.Err())
	}
	finalMsg := stream.Message()
	if len(finalMsg.Parts) != 1 || finalMsg.Parts[0].Image == nil {
		t.Fatalf("Message() did not carry image: %+v", finalMsg)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestExtractPromptAndRefs_MergesUserTextAndDropsOtherRoles(t *testing.T) {
	msgs := []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: "ignored"}}},
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartText, Text: "first"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://ref/a.png"}},
		}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "ignored too"}}},
		{Role: model.RoleUser, Parts: []model.Part{
			{Type: model.PartText, Text: "second"},
		}},
	}
	prompt, refs := extractPromptAndRefs(msgs)
	if prompt != "first\nsecond" {
		t.Errorf("prompt: %q", prompt)
	}
	if len(refs) != 1 || refs[0] != "https://ref/a.png" {
		t.Errorf("refs: %+v", refs)
	}
}

func TestProviderRegistration(t *testing.T) {
	// init() registered the catalog entry for "minimax-image" /
	// "image-01"; the resolver should be able to look it up.
	if _, ok := llm.DefaultRegistry.LookupModel(providerKey, "image-01"); !ok {
		t.Fatal("image-01 not registered under minimax-image provider")
	}
	if _, ok := llm.DefaultRegistry.LookupModel(providerKey, "image-01-live"); !ok {
		t.Fatal("image-01-live not registered under minimax-image provider")
	}
	spec := llm.DefaultRegistry.LookupModelSpec(providerKey, "image-01")
	if spec.Caps.Supports(llm.CapTools) {
		t.Error("expected CapTools to be disabled for image-01")
	}
	if !spec.Caps.Supports(llm.CapImageOutput) {
		t.Error("expected CapImageOutput to be ENABLED for image-01")
	}
}
