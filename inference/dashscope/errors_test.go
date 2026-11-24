package dashscope

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The dashboard's codes are the platform's vocabulary, and the classification is
// what a caller acts on: a throttled call is worth waiting out, a key or an
// account problem is not, and a refused request will be refused again.
func TestClassifyCode(t *testing.T) {
	cases := []struct {
		code  string
		check func(error) bool
		kind  string
	}{
		{"InvalidApiKey", errdefs.IsUnauthorized, "Unauthorized"},
		{"Arrearage", errdefs.IsForbidden, "Forbidden"},
		{"AccessDenied", errdefs.IsForbidden, "Forbidden"},
		{"Throttling.RateQuota", errdefs.IsRateLimit, "RateLimit"},
		{"InvalidParameter", errdefs.IsValidation, "Validation"},
		{"DataInspectionFailed", errdefs.IsValidation, "Validation"},
		{"IPInfringementSuspect", errdefs.IsValidation, "Validation"},
		{"InvalidTaskId", errdefs.IsValidation, "Validation"},
		{"InvalidFile.DownloadFailed", errdefs.IsValidation, "Validation"},
		{"UnsupportedModel", errdefs.IsValidation, "Validation"},
		{"InternalError", errdefs.IsNotAvailable, "NotAvailable"},
		{"ServiceUnavailable", errdefs.IsNotAvailable, "NotAvailable"},
	}
	for _, test := range cases {
		t.Run(test.code, func(t *testing.T) {
			err := classifyCode(test.code, errors.New("base"))
			if !test.check(err) {
				t.Fatalf("%s classified as %v, want %s", test.code, err, test.kind)
			}
			if !strings.Contains(err.Error(), "base") {
				t.Errorf("the cause was dropped: %v", err)
			}
		})
	}

	// A code this driver does not know leaves the error as it was: the status
	// still carries the family, and inventing one would be worse than passing it
	// through.
	base := errors.New("unknown")
	if got := classifyCode("SomethingNew", base); !errors.Is(got, base) {
		t.Errorf("unknown code = %v, want the error unchanged", got)
	}
}

// The status is the fallback, and the envelope refines it: a 400 whose code says
// "throttling" is a rate limit, not a malformed request.
func TestClassifyStatus(t *testing.T) {
	if err := classifyStatus(401, "", errors.New("x")); !errdefs.IsUnauthorized(err) {
		t.Errorf("401 = %v, want Unauthorized", err)
	}
	if err := classifyStatus(503, "", errors.New("x")); !errdefs.IsNotAvailable(err) {
		t.Errorf("503 = %v, want NotAvailable", err)
	}
	refined := classifyStatus(400, "Throttling.AllocationQuota", errors.New("x"))
	if !errdefs.IsRateLimit(refined) {
		t.Errorf("400 + throttling = %v, want RateLimit", refined)
	}
	unknown := classifyStatus(400, "SomethingNew", errors.New("x"))
	if !errdefs.IsValidation(unknown) {
		t.Errorf("400 + unknown code = %v, want the status's Validation", unknown)
	}
}

// failure renders the envelope a deployment's log reads, and carries the request
// id the platform's support asks for.
func TestErrorBodyFailure(t *testing.T) {
	err := errorBody{
		Code:      "InvalidParameter",
		Message:   "  The parameter `size` is invalid.  ",
		RequestID: "req-42",
	}.failure("image", 400)
	if !errdefs.IsValidation(err) {
		t.Fatalf("classification = %v", err)
	}
	text := err.Error()
	for _, want := range []string{"invalidparameter", "The parameter `size` is invalid."} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("message = %q, want it to carry %q", text, want)
		}
	}
	if id, ok := errdefs.RequestID(err); !ok || id != "req-42" {
		t.Errorf("request id = %q (%v), want req-42", id, ok)
	}

	// An envelope with neither a code nor a message still says which call failed
	// and what the status was.
	bare := errorBody{}.failure("video", 502)
	if !strings.Contains(bare.Error(), "HTTP 502") || !strings.Contains(bare.Error(), "no message") {
		t.Errorf("bare envelope = %q", bare.Error())
	}
	if id, ok := errdefs.RequestID(bare); ok || id != "" {
		t.Errorf("request id = %q (%v), want none", id, ok)
	}
}
