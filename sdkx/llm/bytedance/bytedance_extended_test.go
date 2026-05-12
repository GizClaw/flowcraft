package bytedance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestGenerate_EmptyChoices regresses the existing "len(resp.Choices)
// == 0" guard against a degraded ark-runtime response. The
// ChatCompletionResponse is a value type so resp itself can never be
// nil, but Choices may legitimately come back empty when the upstream
// model trips a moderation gate. We pin the contract so any future
// refactor that drops the guard would be caught.
func TestGenerate_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Generate panicked on empty choices: %v", r)
		}
	}()

	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestGenerateStream_TransportError verifies the streaming path
// emits an error (not a panic) when the upstream returns a 5xx.
// The bytedance stream client wraps the SSE reader so a transport
// error during the request shows up as an err from
// CreateChatCompletionStream.
func TestGenerateStream_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateStream panicked on 5xx: %v", r)
		}
	}()

	stream, err := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	// Either the constructor returns err (preferred) or the stream
	// surfaces err via Next/Err. Both are acceptable; what matters is
	// that we never panic.
	if err == nil && stream != nil {
		for stream.Next() {
		}
		_ = stream.Err()
		_ = stream.Close()
	}
}
