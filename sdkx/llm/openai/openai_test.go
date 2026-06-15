package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/openai/openai-go/option"
)

func TestNew_UsesResponsesAPI(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := New("gpt-test", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, usage, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "ok" || usage.TotalTokens != 2 {
		t.Fatalf("response = %q usage=%+v", msg.Content(), usage)
	}
	if got["model"] != "gpt-test" {
		t.Fatalf("request body = %#v", got)
	}
}

func TestOpenAIChatProviderUsesChatCompletionsAPI(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	c, err := llm.NewFromConfig("openai-chat", "gpt-test", map[string]any{
		"api_key":  "test-key",
		"base_url": srv.URL,
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	msg, usage, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "ok" || usage.TotalTokens != 2 || got["model"] != "gpt-test" {
		t.Fatalf("response=%q usage=%+v body=%#v", msg.Content(), usage, got)
	}
}

func TestProviderCatalogsSplitBySurface(t *testing.T) {
	for _, tc := range []struct {
		provider string
		model    string
	}{
		{"openai", "gpt-5.5"},
		{"openai-chat", "gpt-5.5"},
		{"openai", "o3-pro"},
		{"openai-chat", "o4-mini"},
	} {
		if _, ok := llm.DefaultRegistry.LookupModel(tc.provider, tc.model); !ok {
			t.Fatalf("%s/%s not registered", tc.provider, tc.model)
		}
	}

	if _, ok := llm.DefaultRegistry.LookupModel("openai-chat", "o3-pro"); ok {
		t.Fatal("openai-chat/o3-pro must not be registered")
	}
	if spec := llm.DefaultRegistry.LookupModelSpec("openai-chat", "o3-pro"); !spec.IsZero() {
		t.Fatalf("openai-chat/o3-pro spec = %+v, want zero", spec)
	}
}

// TestGenerate_NilResp_NoPanic is the regression test for the
// production panic at line 314 of openai.go (originally reported via
// the LongMemEval _s eval crash at ~9% ingest). The openai-go SDK
// returns (nil, nil) when the upstream OpenAI-compatible backend
// answers with a literal JSON `null` payload — a real-world failure
// mode observed against DeepSeek's /v1/chat/completions during
// degraded operation.
//
// Before the fix, dereferencing resp.Choices crashed the entire
// goroutine. The fix adds an `if resp == nil` guard right after the
// err check; this test pins the contract so the guard never gets
// removed by a future refactor.
func TestGenerate_NilResp_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "null")
	}))
	defer srv.Close()

	c, err := NewChat("test-model", "test-key", srv.URL)
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
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error message should mention nil response, got %q", err.Error())
	}
}

// TestGenerate_EmptyChoices verifies the adjacent defensive branch
// (resp.Choices empty) still works. This branch existed before the
// fix and was the original sentinel; the new resp==nil check sits
// just above it.
func TestGenerate_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	c, err := NewChat("test-model", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable kind, got %v", err)
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error message should mention choices, got %q", err.Error())
	}
}

func TestGenerate_ContextCanceledPreservesAbortedClassification(t *testing.T) {
	c, err := NewChat("test-model", "test-key", "http://127.0.0.1:1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = c.Generate(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsAborted(err) {
		t.Fatalf("expected Aborted kind, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error lost context.Canceled identity: %v", err)
	}
}

func TestGenerate_ContextDeadlinePreservesTimeoutClassification(t *testing.T) {
	c, err := NewChat("test-model", "test-key", "http://127.0.0.1:1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, _, err = c.Generate(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expected Timeout kind, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error lost context.DeadlineExceeded identity: %v", err)
	}
}

func TestGenerateStream_ContextCanceledPreservesAbortedClassification(t *testing.T) {
	c, err := NewChat("test-model", "test-key", "http://127.0.0.1:1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.GenerateStream(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsAborted(err) {
		t.Fatalf("expected Aborted kind, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error lost context.Canceled identity: %v", err)
	}
}

func TestGenerateStream_ContextDeadlinePreservesTimeoutClassification(t *testing.T) {
	c, err := NewChat("test-model", "test-key", "http://127.0.0.1:1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err = c.GenerateStream(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expected Timeout kind, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error lost context.DeadlineExceeded identity: %v", err)
	}
}

// TestGenerateStream_NilStream covers the streaming sibling of the
// nil-resp guard. We simulate a transport-level dial failure by
// pointing the client at a closed socket so NewStreaming gives back
// a handle whose Err() is non-nil, then verify the retry path also
// fails cleanly (no panic) when the secondary call would have
// produced the same failure.
//
// We cannot trivially provoke an actual `nil` stream handle through
// the SDK (NewStreaming always allocates the struct), but the guard
// is symmetric with Generate's. If a future SDK upgrade or panic-
// recovery path starts returning nil, this test will fail loudly.
func TestGenerateStream_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c, err := NewChat("test-model", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateStream panicked: %v", r)
		}
	}()

	_, err = c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestNew_AcceptsExtraOpts pins the constructor extension point so
// tests (and downstream wrappers like deepseek / qwen) keep working.
func TestNew_AcceptsExtraOpts(t *testing.T) {
	_, err := NewChat("m", "k", "https://example.com/v1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New rejected option.WithMaxRetries(0): %v", err)
	}
}
