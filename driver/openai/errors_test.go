package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"

	"github.com/openai/openai-go/v3"
)

func TestProviderSummary(t *testing.T) {
	cases := []struct {
		name   string
		apiErr *openai.Error
		want   string
	}{
		{
			name: "code and message",
			apiErr: &openai.Error{
				StatusCode: http.StatusForbidden,
				Code:       "model_access_denied",
				Message:    "authorized Decisions model or pricing unavailable",
			},
			want: "openai: HTTP 403: model_access_denied: authorized Decisions model or pricing unavailable",
		},
		{
			name: "message only",
			apiErr: &openai.Error{
				StatusCode: http.StatusBadRequest,
				Message:    "Invalid type for 'input'",
			},
			want: "openai: HTTP 400: Invalid type for 'input'",
		},
		{
			name: "code only",
			apiErr: &openai.Error{
				StatusCode: http.StatusTooManyRequests,
				Code:       "rate_limit_exceeded",
			},
			want: "openai: HTTP 429: rate_limit_exceeded",
		},
		{
			name: "message that names its own code",
			apiErr: &openai.Error{
				StatusCode: http.StatusForbidden,
				Code:       "model_access_denied",
				Message:    "model_access_denied: no grant for this surface",
			},
			want: "openai: HTTP 403: model_access_denied: no grant for this surface",
		},
		{
			name:   "status alone",
			apiErr: &openai.Error{StatusCode: http.StatusBadGateway},
			want:   "openai: HTTP 502",
		},
		{
			name: "multi-line message folds onto one line",
			apiErr: &openai.Error{
				StatusCode: http.StatusUnprocessableEntity,
				Code:       "invalid_request_error",
				Message:    "line one\n\tline two\r\nline three",
			},
			want: "openai: HTTP 422: invalid_request_error: line one line two line three",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerSummary(tc.apiErr); got != tc.want {
				t.Fatalf("providerSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProviderSummaryBoundsDetail pins the bound: a provider is free to answer
// a refusal with a whole validation dump, and the text lands in logs and events
// rather than in a transcript.
func TestProviderSummaryBoundsDetail(t *testing.T) {
	message := strings.Repeat("é", providerDetailLimit+50)
	got := providerSummary(&openai.Error{
		StatusCode: http.StatusBadRequest,
		Message:    message,
	})
	want := "openai: HTTP 400: " + strings.Repeat("é", providerDetailLimit) + "..."
	if got != want {
		t.Fatalf("providerSummary = %q, want the message cut at %d runes", got, providerDetailLimit)
	}
	// The cut is rune-aware, not byte-aware: no broken encoding.
	if !strings.HasSuffix(got, "é...") {
		t.Fatalf("providerSummary = %q, want the cut on a rune boundary", got)
	}
}

// TestClassifyErrorCarriesProviderEnvelope drives one refusal through the
// generate path, the way a deployment meets it: the classification stays what
// the runtime acts on, and the text carries the gateway's own code and message
// instead of the SDK's status-only summary.
func TestClassifyErrorCarriesProviderEnvelope(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("x-request-id", "req-gateway")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":"model_access_denied",`+
			`"message":"authorized Decisions model or pricing unavailable",`+
			`"type":"invalid_request_error"}}`)
	})
	defer server.Close()
	cls := testClients(t, server)
	operations, err := openGenerateFor(
		cls,
		declarations["gpt-5.6-sol"],
		openaiModel("gpt-5.6-sol").ID,
		"default",
	)
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	_, err = operations.Unary.Execute(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
	)
	if err == nil {
		t.Fatal("Execute succeeded, want a classified error")
	}
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("error = %v, want ProviderFailure", err)
	}
	if !errdefs.IsForbidden(err) {
		t.Fatalf("classified error = %v, want forbidden", err)
	}
	text := causeChainText(err)
	want := "openai: HTTP 403: model_access_denied: authorized Decisions model or pricing unavailable"
	if !strings.Contains(text, want) {
		t.Fatalf("chain text = %q, want it to carry %q", text, want)
	}
	// The SDK error stays reachable, and the wire facts keep riding along.
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("chain = %v, want the vendor error reachable", err)
	}
	if apiErr.Code != "model_access_denied" {
		t.Fatalf("apiErr.Code = %q, want the envelope's code", apiErr.Code)
	}
	if id, ok := errdefs.RequestID(err); !ok || id != "req-gateway" {
		t.Fatalf("RequestID = %q/%v, want req-gateway/true", id, ok)
	}
}

// causeChainText concatenates the messages of every error in the chain, which
// is how a caller that reads a provider failure sees it.
func causeChainText(err error) string {
	var parts []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		parts = append(parts, current.Error())
	}
	return strings.Join(parts, ": ")
}
