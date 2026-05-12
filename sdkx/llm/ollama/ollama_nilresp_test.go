package ollama

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// nilRespRoundTripper violates the net/http contract on purpose by
// returning (nil, nil). Note that *http.Client.Do itself intercepts
// the violation and turns it into a synthetic error
// ("http: RoundTripper implementation returned a nil *Response with
// a nil error"), so the in-package `if resp == nil` guard added to
// Generate / GenerateStream is unreachable through the standard
// Client. We keep both the guard and this test because:
//
//  1. Future Go versions could relax the stdlib check, or callers
//     may swap in an *http.Client subclass that does not perform it.
//  2. Symmetric handling across providers (openai / anthropic /
//     bytedance / *image / ollama) reduces the cognitive load of
//     "which provider crashes on which malformed upstream response".
//
// The test asserts the surfacing behaviour we DO get today: a clean
// ProviderError from ClassifyProviderError, no panic, error message
// retains the round-trip context.
type nilRespRoundTripper struct{}

func (nilRespRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, nil
}

func newOllamaWithNilResp(t *testing.T) *LLM {
	t.Helper()
	c, err := New("llama3", "http://example.invalid")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.httpClient = &http.Client{Transport: nilRespRoundTripper{}}
	return c
}

// TestGenerate_BadTransport_NoPanic regresses the family-wide
// commitment that no chat-completion provider panics on a misbehaving
// transport. Stdlib turns the (nil, nil) RoundTripper return into a
// synthetic error, so the Generate err-branch handles it cleanly.
func TestGenerate_BadTransport_NoPanic(t *testing.T) {
	c := newOllamaWithNilResp(t)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Generate panicked on bad transport: %v", r)
		}
	}()

	_, _, err := c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "RoundTripper") && !strings.Contains(err.Error(), "nil http response") {
		t.Errorf("error should mention transport violation, got %q", err.Error())
	}
}

// TestGenerateStream_BadTransport_NoPanic is the streaming sibling.
func TestGenerateStream_BadTransport_NoPanic(t *testing.T) {
	c := newOllamaWithNilResp(t)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateStream panicked on bad transport: %v", r)
		}
	}()

	_, err := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "RoundTripper") && !strings.Contains(err.Error(), "nil http response") {
		t.Errorf("error should mention transport violation, got %q", err.Error())
	}
}
