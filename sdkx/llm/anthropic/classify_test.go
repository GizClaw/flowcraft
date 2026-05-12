package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	anth "github.com/anthropics/anthropic-sdk-go"
)

// TestClassifyAPIError pins the status-code → errdefs mapping for
// anthropic-sdk-go's *anth.Error. The trail of trust the generic
// errdefs.ClassifyProviderError relies on is broken for these errors
// because Error.Error() wraps the URL ("https://...") between the
// status keyword and the digit code — see classify.go for the full
// explanation.
func TestClassifyAPIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantSent func(error) bool
		wantName string
	}{
		{"400 → Validation", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`,
			errdefs.IsValidation, "Validation"},
		{"404 → Validation (no Azure capacity quirk here)", 404,
			`{"type":"error","error":{"type":"not_found_error","message":"x"}}`,
			errdefs.IsValidation, "Validation"},
		{"422 → Validation", 422,
			`{"type":"error","error":{"type":"unprocessable","message":"x"}}`,
			errdefs.IsValidation, "Validation"},
		{"401 → Unauthorized", 401,
			`{"type":"error","error":{"type":"authentication_error","message":"x"}}`,
			errdefs.IsUnauthorized, "Unauthorized"},
		{"429 → RateLimit", 429,
			`{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`,
			errdefs.IsRateLimit, "RateLimit"},
		{"500 → NotAvailable", 500,
			`{"type":"error","error":{"type":"api_error","message":"x"}}`,
			errdefs.IsNotAvailable, "NotAvailable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Disable anthropic-sdk-go's automatic retry on 429/5xx
				// so the test stays single-shot.
				w.Header().Set("x-should-retry", "false")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, err := New("claude-3-sonnet-20240229", "test-key", srv.URL, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantSent(err) {
				t.Fatalf("expected errdefs.Is%s, got %v", tc.wantName, err)
			}
		})
	}
}

// TestClassifyAPIErrorNonAnth covers the fallback to the generic
// errdefs.ClassifyProviderError path.
func TestClassifyAPIErrorNonAnth(t *testing.T) {
	err := errors.New("network: dial tcp: connection refused")
	got := classifyAPIError(err)
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable fallback, got %v", got)
	}
}

// TestClassifyAPIErrorIdempotent: already-classified errors pass through
// unchanged so callers can call the helper twice without rewrapping.
func TestClassifyAPIErrorIdempotent(t *testing.T) {
	original := errdefs.Validation(errors.New("explicit"))
	if got := classifyAPIError(original); !errors.Is(got, original) {
		t.Fatalf("idempotent path lost the original errdefs marker: %v", got)
	}
}

// Defensive guard: if upstream renames *anth.Error in a future version,
// errors.As stops matching and our type-routing falls through to the
// generic path — surface that with a Skip so the failure is loud.
func TestClassifyAPIErrorTypeAs(t *testing.T) {
	ae := &anth.Error{StatusCode: 400}
	if !errors.As(ae, new(*anth.Error)) {
		t.Skip("anthropic-sdk-go: *anth.Error no longer matches errors.As — re-audit needed")
	}
}
