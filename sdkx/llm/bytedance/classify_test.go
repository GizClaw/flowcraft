package bytedance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// TestClassifyAPIError pins the status-code → errdefs mapping for
// arkruntime's APIError. Without this routing the generic
// errdefs.ClassifyProvider's regex misses arkruntime's
// `"Error code: %d - ..."` format (no "http"/"status" keyword in the
// message string), and every 4xx falls through to the NotAvailable
// default — which a caller's retry-once would then quietly retry.
func TestClassifyAPIError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantSent func(error) bool
		wantName string
	}{
		{"400 → Validation", 400,
			`{"error":{"code":"InvalidParameter","message":"x","type":"InvalidParameter"}}`,
			errdefs.IsValidation, "Validation"},
		{"404 → Validation", 404,
			`{"error":{"code":"NotFound","message":"x","type":"NotFound"}}`,
			errdefs.IsValidation, "Validation"},
		{"422 → Validation", 422,
			`{"error":{"code":"Unprocessable","message":"x","type":"Unprocessable"}}`,
			errdefs.IsValidation, "Validation"},
		{"401 → Unauthorized", 401,
			`{"error":{"code":"AuthenticationError","message":"x","type":"AuthenticationError"}}`,
			errdefs.IsUnauthorized, "Unauthorized"},
		{"429 → RateLimit", 429,
			`{"error":{"code":"RateLimitExceeded","message":"x","type":"RateLimitExceeded"}}`,
			errdefs.IsRateLimit, "RateLimit"},
		{"500 → NotAvailable", 500,
			`{"error":{"code":"InternalError","message":"x","type":"InternalError"}}`,
			errdefs.IsNotAvailable, "NotAvailable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			// retry_times=0 keeps the test single-shot — arkruntime
			// has its own retry loop on transient errors and we don't
			// want it amplifying httptest's response count.
			c, err := New("doubao-test", "test-key", srv.URL, "", 0)
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

// TestClassifyAPIError_RequestError covers the second arkruntime error
// shape — RequestError is what the SDK returns when the JSON body
// can't be unmarshaled to APIError (transport-level / proxy errors).
// HTTPStatusCode is still populated, so we route by code the same way.
func TestClassifyAPIError_RequestError(t *testing.T) {
	re := arkmodel.NewRequestError(500, errors.New("upstream boom"), "req-1")
	got := classifyAPIError(re)
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable for RequestError(500), got %v", got)
	}
}

// TestClassifyAPIErrorNonArk: fallback path for non-arkruntime errors.
func TestClassifyAPIErrorNonArk(t *testing.T) {
	got := classifyAPIError(errors.New("net: dial tcp: connection refused"))
	if !errdefs.IsNotAvailable(got) {
		t.Fatalf("expected NotAvailable fallback, got %v", got)
	}
}

// TestClassifyAPIErrorIdempotent: already-classified errors pass through.
func TestClassifyAPIErrorIdempotent(t *testing.T) {
	original := errdefs.Validation(errors.New("explicit"))
	if got := classifyAPIError(original); !errors.Is(got, original) {
		t.Fatalf("idempotent path lost the original errdefs marker: %v", got)
	}
}

func TestCatalogCapsMatchResponsesAPI(t *testing.T) {
	spec := llm.DefaultRegistry.LookupModelSpec("bytedance", "doubao-seed-2-0-lite-260215")
	for _, cap := range []llm.Capability{
		llm.CapStopWords,
		llm.CapFrequencyPenalty,
		llm.CapPresencePenalty,
		llm.CapImageOutput,
		llm.CapAudioOutput,
		llm.CapAudio,
	} {
		if spec.Caps.Supports(cap) {
			t.Fatalf("cap %s is supported, want disabled", cap)
		}
	}
	for _, cap := range []llm.Capability{
		llm.CapTools,
		llm.CapToolChoice,
		llm.CapParallelTools,
		llm.CapStreaming,
		llm.CapJSONMode,
		llm.CapJSONSchema,
	} {
		if !spec.Caps.Supports(cap) {
			t.Fatalf("cap %s is disabled, want supported", cap)
		}
	}
}
