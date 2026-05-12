package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

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
