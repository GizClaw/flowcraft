package errdefs

import (
	"errors"
	"fmt"
	"testing"
)

// httpErr is a fixture that satisfies HTTPStatusCoder so the
// classifier's structured-status-first path can be exercised against
// known codes without depending on an external SDK error type.
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string       { return e.msg }
func (e *httpErr) HTTPStatusCode() int { return e.code }

// TestClassifyProvider pins the structured-error / keyword / regex
// dispatcher. Behaviour change here means every dependent (sdk/llm
// fallback, sdkx/embedding, future sdkx/rerank) silently shifts how it
// classifies the same upstream error.
func TestClassifyProvider(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want ProviderCategory
	}{
		// HTTPStatusCoder takes precedence (most reliable signal).
		{"401 via interface", &httpErr{code: 401, msg: "nope"}, ProviderAuth},
		{"403 via interface", &httpErr{code: 403, msg: "forbid"}, ProviderAuth},
		{"402 via interface", &httpErr{code: 402, msg: "pay"}, ProviderBilling},
		{"429 via interface", &httpErr{code: 429, msg: "slow"}, ProviderRateLimit},
		{"400 via interface", &httpErr{code: 400, msg: "bad"}, ProviderPermanent},
		{"422 via interface", &httpErr{code: 422, msg: "unproc"}, ProviderPermanent},
		{"500 via interface (transient)", &httpErr{code: 500, msg: "boom"}, ProviderTransient},
		{"wrapped HTTPStatusCoder", fmt.Errorf("op: %w", &httpErr{code: 429, msg: "x"}), ProviderRateLimit},

		// Keyword scan over the joined chain message.
		{"context overflow keyword", fmt.Errorf("maximum context length exceeded"), ProviderContextOverflow},
		{"too many tokens keyword", fmt.Errorf("too many tokens in request"), ProviderContextOverflow},
		{"auth keyword", fmt.Errorf("invalid api key provided"), ProviderAuth},
		{"billing keyword", fmt.Errorf("insufficient credits"), ProviderBilling},
		{"rate limit keyword", fmt.Errorf("rate limit exceeded"), ProviderRateLimit},

		// HTTP code embedded in message — last-resort fallback.
		{"http 401 in message", fmt.Errorf("status 401 from upstream"), ProviderAuth},
		{"http 429 in message", fmt.Errorf("got http 429"), ProviderRateLimit},

		// Keyword overrides ambiguous HTTP code: "http 400: max context"
		// must NOT classify as Permanent — the inner cause is overflow,
		// which is what FallbackLLM uses to skip the chain.
		{"400+overflow keyword wins", fmt.Errorf("http 400: maximum context length exceeded"), ProviderContextOverflow},
		{"400+auth keyword wins", fmt.Errorf("http 400: unauthorized access"), ProviderAuth},

		// Multi-error chain (errors.Join) must still match through the
		// inner error message. This is the path sdk/llm uses when it
		// joins ctx.Err() with the SDK error before logging.
		{"joined errors", errors.Join(fmt.Errorf("first"), fmt.Errorf("rate limit exceeded")), ProviderRateLimit},

		// Default bucket — used by FallbackLLM to decide retry.
		{"plain network error", fmt.Errorf("connection refused"), ProviderTransient},
		{"empty error", fmt.Errorf(""), ProviderTransient},
		{"nil", nil, ProviderTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyProvider(tc.in); got != tc.want {
				t.Errorf("ClassifyProvider(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestClassifyProviderError_Mapping pins the ProviderCategory →
// errdefs class mapping. pod policies branch on the IsXxx predicates,
// so any drift here is a behaviour change for every operator running
// on top of the SDK.
func TestClassifyProviderError_Mapping(t *testing.T) {
	cases := []struct {
		name  string
		in    error
		check func(error) bool
	}{
		{"auth → Unauthorized", &httpErr{code: 401, msg: "x"}, IsUnauthorized},
		{"billing → Forbidden", &httpErr{code: 402, msg: "x"}, IsForbidden},
		{"rate limit → RateLimit", &httpErr{code: 429, msg: "x"}, IsRateLimit},
		{"permanent → Validation", &httpErr{code: 400, msg: "x"}, IsValidation},
		{"context overflow → Validation", fmt.Errorf("maximum context length exceeded"), IsValidation},
		{"transient → NotAvailable", fmt.Errorf("connection refused"), IsNotAvailable},
		{"500 → NotAvailable", &httpErr{code: 500, msg: "x"}, IsNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyProviderError("openai", tc.in)
			if !tc.check(got) {
				t.Errorf("predicate did not match, got %v (HTTPStatus=%d)", got, HTTPStatus(got))
			}
			if !errors.Is(got, tc.in) {
				t.Errorf("wrap broke errors.Is chain")
			}
		})
	}
}

// TestClassifyProviderError_NilPassthrough lets providers call the
// helper unconditionally on every err return without inventing a
// nil-vs-non-nil branch.
func TestClassifyProviderError_NilPassthrough(t *testing.T) {
	if got := ClassifyProviderError("openai", nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestClassifyProviderError_PreservesExistingClassification covers the
// "ctx.Err() already wrapped into errdefs.Timeout" case. If the
// provider passes such an error through ClassifyProviderError we must
// not downgrade Timeout to NotAvailable: the timeout vs. server-down
// distinction is what drives whether pod retries.
func TestClassifyProviderError_PreservesExistingClassification(t *testing.T) {
	original := Timeoutf("openai.generate: 30s")
	got := ClassifyProviderError("openai", original)
	if !IsTimeout(got) {
		t.Errorf("Timeout was overwritten: got %v", got)
	}
	if IsNotAvailable(got) {
		t.Errorf("Timeout should not also report NotAvailable: %v", got)
	}
}

// TestClassifyHTTPStatus_Mapping pins the raw-HTTP-status path that
// providers like ollama use when they drive their own HTTP client.
// Must stay in lock-step with TestClassifyProviderError_Mapping so
// routing is independent of whether the upstream SDK exposes a typed
// error or just a status code.
func TestClassifyHTTPStatus_Mapping(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		check func(error) bool
	}{
		{"401 → Unauthorized", 401, IsUnauthorized},
		{"403 → Unauthorized", 403, IsUnauthorized},
		{"402 → Forbidden", 402, IsForbidden},
		{"429 → RateLimit", 429, IsRateLimit},
		{"400 → Validation", 400, IsValidation},
		{"422 → Validation", 422, IsValidation},
		{"500 → NotAvailable", 500, IsNotAvailable},
		{"503 → NotAvailable", 503, IsNotAvailable},
		// Unknown 4xx falls through to Internal — caller is meant to
		// pre-gate on a non-2xx response anyway, so this is purely a
		// "can't happen" safety net rather than a routing hint.
		{"418 → Internal", 418, IsInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHTTPStatus("ollama", tc.code, "body")
			if !tc.check(got) {
				t.Errorf("status %d: predicate did not match, got %v (HTTPStatus=%d)",
					tc.code, got, HTTPStatus(got))
			}
		})
	}
}

// TestProviderCategoryFromHTTPCode pins the bare code → category map
// that both ClassifyProvider and ClassifyHTTPStatus use. Calling it
// directly is exposed so consumers like sdk/llm fallback can map a
// status code to a cooldown class without paying for the keyword scan.
func TestProviderCategoryFromHTTPCode(t *testing.T) {
	cases := []struct {
		code int
		want ProviderCategory
		ok   bool
	}{
		{401, ProviderAuth, true},
		{403, ProviderAuth, true},
		{402, ProviderBilling, true},
		{429, ProviderRateLimit, true},
		{400, ProviderPermanent, true},
		{404, ProviderPermanent, true},
		{405, ProviderPermanent, true},
		{422, ProviderPermanent, true},
		// Untracked codes return (Transient, false). The bool tells
		// callers "no opinion" so they can apply their own default.
		{500, ProviderTransient, false},
		{418, ProviderTransient, false},
		{200, ProviderTransient, false},
	}
	for _, tc := range cases {
		got, ok := ProviderCategoryFromHTTPCode(tc.code)
		if got != tc.want || ok != tc.ok {
			t.Errorf("code %d: got (%d, %v), want (%d, %v)",
				tc.code, got, ok, tc.want, tc.ok)
		}
	}
}
