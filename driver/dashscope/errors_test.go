package dashscope

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The envelopes behind these cases are the platform's own answers, captured
// against the live endpoint: the code it sent, the status it arrived under,
// and the class the driver owes the caller for it.
//
// The rows with status 0 are the in-band failures — a task that reaches FAILED
// and an event that carries a refusal inside an HTTP 200 — where no status is
// involved, so the code table is the only thing that can classify them.
func TestClassifyPlatformEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   string
		status int
		check  func(error) bool
	}{
		{
			"synchronous call on a task surface",
			"AccessDenied", http.StatusForbidden, errdefs.IsForbidden,
		},
		{
			"key from another region",
			"InvalidApiKey", http.StatusUnauthorized, errdefs.IsUnauthorized,
		},
		{
			"parameter refused by the sync surface",
			"InvalidParameter", http.StatusBadRequest, errdefs.IsValidation,
		},
		{
			"model that does not exist",
			"InvalidParameter", http.StatusBadRequest, errdefs.IsValidation,
		},
		{
			"task-level parameter failure",
			"InvalidParameter", 0, errdefs.IsValidation,
		},
		{
			"content flagged by inspection",
			"DataInspectionFailed", 0, errdefs.IsValidation,
		},
		{
			"task id the platform does not know",
			"InvalidTaskId", 0, errdefs.IsValidation,
		},
		{
			"account in arrears",
			"Arrearage", http.StatusForbidden, errdefs.IsForbidden,
		},
		{
			"throttled",
			"Throttling", http.StatusTooManyRequests, errdefs.IsRateLimit,
		},
		{
			"platform failing",
			"InternalError", http.StatusInternalServerError, errdefs.IsNotAvailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := errorBody{
				Code:      tc.code,
				Message:   "the platform's own words",
				RequestID: "req-live",
			}.failure("probe", tc.status)
			if !tc.check(err) {
				t.Errorf("class = %v, want %s", err, tc.name)
			}
			if id, ok := errdefs.RequestID(err); !ok || id != "req-live" {
				t.Errorf("request id = %q (%v), want the envelope's id", id, ok)
			}
			if detail := err.Error(); !strings.Contains(detail, tc.code) {
				t.Errorf("error = %q, want the platform code in it", detail)
			}
		})
	}
}

// TestClassifyFallsBackToStatus pins the other half of the mapping: a code the
// table has never heard of still lands in the right family when the status is
// expressive, so an unfamiliar platform code degrades to the transport truth
// rather than to no classification at all.
func TestClassifyFallsBackToStatus(t *testing.T) {
	err := classifyStatus(
		http.StatusForbidden,
		"SomeCodeFromTheFuture",
		errors.New("dashscope: probe failed"),
	)
	if !errdefs.IsForbidden(err) {
		t.Errorf("class = %v, want forbidden from the status", err)
	}
}
