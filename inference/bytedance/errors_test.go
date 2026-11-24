package bytedance

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The Ark surfaces report failures as a code and a message, so the mapping is a
// reading of both. What a caller acts on is the category: a credential is
// replaced, a refusal is accepted, a bad request is fixed, a throttle is waited
// out, and anything unrecognised is the far end being unwell.
//
// Every term in the table is exercised here. A term nothing tests is a term a
// typo can silently disable.
func TestClassifyResponseError(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
		check   func(error) bool
		kind    string
	}{
		// The credential.
		{name: "bad key", code: "AuthenticationError",
			message: "The API key is invalid.", check: errdefs.IsUnauthorized, kind: "Unauthorized"},
		{name: "key code", code: "InvalidApiKey",
			message: "unknown", check: errdefs.IsUnauthorized, kind: "Unauthorized"},
		{name: "unauthorized code", code: "Unauthorized",
			message: "plain", check: errdefs.IsUnauthorized, kind: "Unauthorized"},
		{name: "auth failed", code: "AuthFailed",
			message: "plain", check: errdefs.IsUnauthorized, kind: "Unauthorized"},
		{name: "prose credential", code: "Unknown.Code",
			message: "The provided credential is not valid.", check: errdefs.IsUnauthorized,
			kind: "Unauthorized"},
		{name: "prose api key", code: "",
			message: "api key expired", check: errdefs.IsUnauthorized, kind: "Unauthorized"},

		// The account may not do this.
		{name: "access denied", code: "AccessDenied",
			message: "call is not allowed", check: errdefs.IsForbidden, kind: "Forbidden"},
		{name: "account overdue", code: "AccountOverdue",
			message: "recharge your account", check: errdefs.IsForbidden, kind: "Forbidden"},
		{name: "arrearage", code: "Arrearage",
			message: "unpaid bill", check: errdefs.IsForbidden, kind: "Forbidden"},
		{name: "balance", code: "InsufficientBalance",
			message: "top up", check: errdefs.IsForbidden, kind: "Forbidden"},
		{name: "quota", code: "QuotaExceeded",
			message: "The account quota for this model is exhausted.",
			check:   errdefs.IsForbidden, kind: "Forbidden"},
		{name: "access denied prose", code: "SomethingNew",
			message: "Access denied for this endpoint", check: errdefs.IsForbidden,
			kind: "Forbidden"},
		{name: "quota prose", code: "SomethingNew",
			message: "the quota is exhausted for this account",
			check:   errdefs.IsForbidden, kind: "Forbidden"},

		// The far end is asking for a slower caller, and a spent per-minute
		// allowance is the one quota that reads this way.
		{name: "rate code", code: "RateLimitExceeded",
			message: "You have exceeded the request rate limit.",
			check:   errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "separated code", code: "rate_limit_exceeded",
			message: "plain", check: errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "throttling", code: "Throttling",
			message: "plain", check: errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "too many requests", code: "TooManyRequests",
			message: "plain", check: errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "overloaded", code: "Overloaded",
			message: "plain", check: errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "prose rate limit", code: "SomethingNew",
			message: "you have hit the rate limit for this model",
			check:   errdefs.IsRateLimit, kind: "RateLimit"},
		{name: "quota with a rate", code: "QuotaExceeded",
			message: "rate limit reached for this model, retry later",
			check:   errdefs.IsRateLimit, kind: "RateLimit"},

		// The request itself is wrong.
		{name: "invalid parameter", code: "InvalidParameter",
			message: "The parameter `size` is not valid.", check: errdefs.IsValidation,
			kind: "Validation"},
		{name: "not found", code: "ResourceNotFound",
			message: "plain", check: errdefs.IsValidation, kind: "Validation"},
		{name: "prose not found", code: "SomethingNew",
			message: "the endpoint was not found", check: errdefs.IsValidation, kind: "Validation"},
		{name: "model not open", code: "ModelNotOpen",
			message: "plain", check: errdefs.IsValidation, kind: "Validation"},
		{name: "unsupported model", code: "UnsupportedModel",
			message: "plain", check: errdefs.IsValidation, kind: "Validation"},
		{name: "sensitive output", code: "OutputImageSensitiveContentDetected",
			message: "The generated image was flagged.", check: errdefs.IsValidation,
			kind: "Validation"},
		{name: "prose sensitive", code: "SomethingNew",
			message: "the result contains sensitive content", check: errdefs.IsValidation,
			kind: "Validation"},
		{name: "filter failed", code: "InputTextFilterFailed",
			message: "plain", check: errdefs.IsValidation, kind: "Validation"},
		{name: "prose content filter", code: "SomethingNew",
			message: "rejected by the content filter", check: errdefs.IsValidation,
			kind: "Validation"},

		// Nothing recognised: the far end is unwell, which is what a retry is for.
		{name: "internal", code: "InternalServiceError",
			message: "the service is temporarily unavailable",
			check:   errdefs.IsNotAvailable, kind: "NotAvailable"},
		// The words that used to hijack a category: "generated" contains "rate",
		// "unlimited" contains "limit", "asterisk" is not a risk, "notfound" is
		// spelled in code form above and the prose form here.
		{name: "generated", code: "OutputImageGenerationFailed",
			message: "The system failed to generate the image.",
			check:   errdefs.IsNotAvailable, kind: "NotAvailable"},
		{name: "unlimited", code: "SomethingNew",
			message: "unlimited concurrency is not available here",
			check:   errdefs.IsNotAvailable, kind: "NotAvailable"},
		{name: "asterisk", code: "SomethingNew",
			message: "unexpected asterisk in the prompt",
			check:   errdefs.IsNotAvailable, kind: "NotAvailable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := classifyResponseError(test.code, test.message)
			if err == nil {
				t.Fatal("a failure classified as nil")
			}
			if !test.check(err) {
				t.Fatalf("%s %q classified as %v, want %s",
					test.code, test.message, err, test.kind)
			}
			// The text is what a human debugging a deployment reads, so the code
			// and message have to survive classification.
			if test.code != "" && !strings.Contains(err.Error(), test.code) {
				t.Errorf("classified error lost the code: %v", err)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Errorf("classified error lost the message: %v", err)
			}
		})
	}
}

// The order the categories are read in is a decision, so it is pinned: a message
// that names both a credential and a throttle is a credential problem, and one
// that names both a rate and a quota is a throttle.
func TestClassifyResponseErrorOrder(t *testing.T) {
	both := classifyResponseError("RateLimitExceeded", "rate limit for this auth tier")
	if !errdefs.IsRateLimit(both) {
		t.Errorf("a throttled credential tier = %v, want RateLimit from the code", both)
	}
	ambiguous := classifyResponseError("QuotaExceeded", "tpm quota exceeded, slow down")
	if !errdefs.IsRateLimit(ambiguous) {
		t.Errorf("a per-minute quota = %v, want RateLimit", ambiguous)
	}
	permanent := classifyResponseError("QuotaExceeded", "the account quota is exhausted")
	if !errdefs.IsForbidden(permanent) {
		t.Errorf("an exhausted plan = %v, want Forbidden", permanent)
	}
}
