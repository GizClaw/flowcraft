package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/openai/openai-go/option"
)

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

	c, err := New("test-model", "test-key", srv.URL)
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

	c, err := New("test-model", "test-key", srv.URL)
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

	c, err := New("test-model", "test-key", srv.URL)
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
	_, err := New("m", "k", "https://example.com/v1", option.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New rejected option.WithMaxRetries(0): %v", err)
	}
}
