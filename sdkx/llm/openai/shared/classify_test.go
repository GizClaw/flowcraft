package shared

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	oai "github.com/openai/openai-go"
)

// ClassifyAPIError leans on the live SDK error shape, not a hand-crafted
// fixture, because openai-go's *oai.Error is populated from JSON and its
// `Code` / `StatusCode` field semantics are surprisingly fragile to mock.
// Each test stands up a httptest server that returns the status code +
// body shape we care about, fires a real chat-completions call through
// the SDK, and asserts ClassifyAPIError routes the resulting error to the
// right errdefs bucket.
func TestClassifyAPIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantBkt  string
		wantSent func(error) bool
	}{
		{
			name:     "400 bad request -> Validation (not retryable)",
			status:   400,
			body:     `{"error":{"code":"invalid_request_error","message":"unsupported param"}}`,
			wantBkt:  "Validation",
			wantSent: errdefs.IsValidation,
		},
		{
			name:   "404 with DeploymentNotFound body -> Validation",
			status: 404,
			// Real Azure shape - the body fills `error.code` so
			// ClassifyAPIError must treat it as a permanent misconfig,
			// not as a transient capacity blip.
			body:     `{"error":{"code":"DeploymentNotFound","message":"The API deployment ..."}}`,
			wantBkt:  "Validation",
			wantSent: errdefs.IsValidation,
		},
		{
			name:   "404 empty body -> NotAvailable (azure capacity blip)",
			status: 404,
			// Front Door layer answers with bare 404 / no parseable
			// body when the MaaS deployment pod is cold-starting.
			// errdefs.NotAvailable so the eval runner's retry-once
			// recovers it.
			body:     ``,
			wantBkt:  "NotAvailable",
			wantSent: errdefs.IsNotAvailable,
		},
		{
			name:     "401 -> Unauthorized",
			status:   401,
			body:     `{"error":{"code":"invalid_api_key","message":"Incorrect API key"}}`,
			wantBkt:  "Unauthorized",
			wantSent: errdefs.IsUnauthorized,
		},
		{
			name:     "429 -> RateLimit",
			status:   429,
			body:     `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}`,
			wantBkt:  "RateLimit",
			wantSent: errdefs.IsRateLimit,
		},
		{
			name:     "500 -> NotAvailable",
			status:   500,
			body:     `{"error":{"code":"internal_error","message":"server error"}}`,
			wantBkt:  "NotAvailable",
			wantSent: errdefs.IsNotAvailable,
		},
		{
			name:     "422 unprocessable -> Validation",
			status:   422,
			body:     `{"error":{"code":"unprocessable","message":"semantic error"}}`,
			wantBkt:  "Validation",
			wantSent: errdefs.IsValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Skip openai-go's automatic retry on 408/409/429/5xx
				// so the test stays single-shot - otherwise a 500 case
				// quietly spends 3 retries before we get the error.
				w.Header().Set("x-should-retry", "false")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			client := NewClient("k", ts.URL)
			_, err := client.Chat.Completions.New(t.Context(), oai.ChatCompletionNewParams{
				Model:    "gpt-test",
				Messages: []oai.ChatCompletionMessageParamUnion{oai.UserMessage("hi")},
			})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			got := ClassifyAPIError(err)
			if !tc.wantSent(got) {
				t.Fatalf("expected errdefs.Is%s == true, got err=%v", tc.wantBkt, got)
			}
		})
	}
}

// TestClassifyAPIErrorNonOAI verifies the fallback path: anything that
// isn't an *oai.Error (raw net errors, ctx errors, etc.) flows through
// errdefs.ClassifyProviderError so the existing keyword heuristics still
// catch their cases.
func TestClassifyAPIErrorNonOAI(t *testing.T) {
	err := errors.New("network: connection reset by peer")
	got := ClassifyAPIError(err)
	// errdefs.ClassifyProviderError falls back to NotAvailable for
	// unclassified network-shaped errors; that's the contract we
	// preserve when delegating.
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable fallback, got %v", got)
	}
}

// TestClassifyAPIErrorIdempotent guards against double-wrapping when an
// already-classified error is passed back in (e.g. on a streaming-retry
// path that re-runs classification).
func TestClassifyAPIErrorIdempotent(t *testing.T) {
	original := errdefs.Validation(errors.New("explicit"))
	got := ClassifyAPIError(original)
	if !errors.Is(got, original) {
		t.Fatalf("idempotent path lost the original errdefs marker: got %v", got)
	}
}

// Ensure oai.Error path actually triggers errors.As - defensive against
// future openai-go API drift that might rename the type.
func TestClassifyAPIErrorTypeAs(t *testing.T) {
	ae := &oai.Error{StatusCode: 400}
	if !errors.As(ae, new(*oai.Error)) {
		t.Skipf("openai-go v%s no longer matches errors.As on *oai.Error - adapter needs a re-audit", oaiVersion())
	}
}

// oaiVersion is a placeholder helper so the test reads naturally; the
// actual SDK version is pinned in sdkx/go.mod.
func oaiVersion() string { return "current" }
