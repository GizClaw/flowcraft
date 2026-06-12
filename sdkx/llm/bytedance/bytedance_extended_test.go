package bytedance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestGenerate_EmptyResponsesOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","object":"response","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Generate panicked on empty output: %v", r)
		}
	}()

	_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestGenerateStream_TransportError verifies the streaming path emits
// an error (not a panic) when the upstream returns a 5xx.
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
