package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected ErrorCategory
	}{
		// HTTP status codes (no keyword overlap)
		{"401 unauthorized", fmt.Errorf("http 401: bad credentials"), CategoryAuth},
		{"403 forbidden", fmt.Errorf("http 403: forbidden"), CategoryAuth},
		{"402 payment", fmt.Errorf("http 402: payment required"), CategoryBilling},
		{"429 throttled", fmt.Errorf("http 429: throttled"), CategoryRateLimit},
		{"400 bad request", fmt.Errorf("http 400: bad request"), CategoryPermanent},
		{"422 unprocessable", fmt.Errorf("http 422: unprocessable"), CategoryPermanent},
		{"404 not found", fmt.Errorf("http 404: not found"), CategoryPermanent},
		{"405 method", fmt.Errorf("http 405: method not allowed"), CategoryPermanent},
		{"500 server error", fmt.Errorf("http 500: internal server error"), CategoryTransient},
		{"502 bad gateway", fmt.Errorf("http 502: bad gateway"), CategoryTransient},
		{"status 429", fmt.Errorf("status 429"), CategoryRateLimit},

		// Keyword matching
		{"invalid api key", fmt.Errorf("invalid api key provided"), CategoryAuth},
		{"incorrect api key", fmt.Errorf("incorrect api key"), CategoryAuth},
		{"authentication failed", fmt.Errorf("authentication error"), CategoryAuth},
		{"permission denied", fmt.Errorf("permission denied for model"), CategoryAuth},
		{"rate limit hit", fmt.Errorf("rate limit exceeded"), CategoryRateLimit},
		{"overloaded", fmt.Errorf("server currently overloaded"), CategoryRateLimit},
		{"server is busy", fmt.Errorf("server is busy, try later"), CategoryRateLimit},
		{"context overflow", fmt.Errorf("maximum context length exceeded"), CategoryContextOverflow},
		{"too many tokens", fmt.Errorf("too many tokens in request"), CategoryContextOverflow},
		{"content too large", fmt.Errorf("content too large"), CategoryContextOverflow},
		{"insufficient credits", fmt.Errorf("insufficient credits"), CategoryBilling},
		{"quota exceeded", fmt.Errorf("quota exceeded for this month"), CategoryBilling},

		// Keywords override HTTP code (core edge cases)
		{"400+context overflow", fmt.Errorf("http 400: maximum context length exceeded"), CategoryContextOverflow},
		{"400+too many tokens", fmt.Errorf("http 400: too many tokens"), CategoryContextOverflow},
		{"400+unauthorized kw", fmt.Errorf("http 400: unauthorized access"), CategoryAuth},
		{"429+rate limit kw", fmt.Errorf("http 429: rate limit exceeded"), CategoryRateLimit},
		{"402+billing kw", fmt.Errorf("http 402: insufficient credits"), CategoryBilling},

		// HTTPStatusCoder interface
		{"HTTPStatusCoder 401", &httpErr{code: 401, msg: "nope"}, CategoryAuth},
		{"HTTPStatusCoder 429", &httpErr{code: 429, msg: "slow down"}, CategoryRateLimit},
		{"HTTPStatusCoder 402", &httpErr{code: 402, msg: "pay up"}, CategoryBilling},
		{"HTTPStatusCoder 400", &httpErr{code: 400, msg: "bad"}, CategoryPermanent},
		{"HTTPStatusCoder 500 fallback to transient", &httpErr{code: 500, msg: "boom"}, CategoryTransient},
		{"HTTPStatusCoder wrapped", fmt.Errorf("outer: %w", &httpErr{code: 429, msg: "limit"}), CategoryRateLimit},

		// Transient fallback
		{"generic network error", fmt.Errorf("connection refused"), CategoryTransient},
		{"timeout", fmt.Errorf("request timed out"), CategoryTransient},
		{"nil error", nil, CategoryTransient},
		{"empty error", fmt.Errorf(""), CategoryTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyError(tt.err)
			if got != tt.expected {
				t.Errorf("ClassifyError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestErrorCategory_String(t *testing.T) {
	tests := []struct {
		cat  ErrorCategory
		want string
	}{
		{CategoryTransient, "transient"},
		{CategoryRateLimit, "rate_limit"},
		{CategoryAuth, "auth"},
		{CategoryBilling, "billing"},
		{CategoryContextOverflow, "context_overflow"},
		{CategoryPermanent, "permanent"},
	}
	for _, tt := range tests {
		if got := tt.cat.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.cat, got, tt.want)
		}
	}
}

func TestErrorCategory_ShouldFallback(t *testing.T) {
	tests := []struct {
		cat    ErrorCategory
		expect bool
	}{
		{CategoryTransient, true},
		{CategoryRateLimit, true},
		{CategoryAuth, true},
		{CategoryBilling, true},
		{CategoryContextOverflow, false},
		{CategoryPermanent, false},
	}
	for _, tt := range tests {
		t.Run(tt.cat.String(), func(t *testing.T) {
			if got := tt.cat.ShouldFallback(); got != tt.expect {
				t.Errorf("%v.ShouldFallback() = %v, want %v", tt.cat, got, tt.expect)
			}
		})
	}
}

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string       { return e.msg }
func (e *httpErr) HTTPStatusCode() int { return e.code }

func TestCollectErrorMessages_JoinedErrors(t *testing.T) {
	err := errors.Join(
		fmt.Errorf("first error"),
		fmt.Errorf("rate limit exceeded"),
	)
	got := ClassifyError(err)
	if got != CategoryRateLimit {
		t.Fatalf("ClassifyError(joined) = %v, want rate_limit", got)
	}
}

func TestCollectErrorMessages_DeepChain(t *testing.T) {
	inner := fmt.Errorf("maximum context length exceeded")
	mid := fmt.Errorf("call failed: %w", inner)
	outer := fmt.Errorf("generate: %w", mid)
	got := ClassifyError(outer)
	if got != CategoryContextOverflow {
		t.Fatalf("ClassifyError(deep chain) = %v, want context_overflow", got)
	}
}

func TestCollectErrorMessages_MixedJoinAndWrap(t *testing.T) {
	wrapped := fmt.Errorf("op: %w", fmt.Errorf("insufficient credits"))
	joined := errors.Join(
		fmt.Errorf("some noise"),
		wrapped,
	)
	got := ClassifyError(joined)
	if got != CategoryBilling {
		t.Fatalf("ClassifyError(mixed) = %v, want billing", got)
	}
}

func TestIsPermanentError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"nil", nil, false},
		{"transient 500", fmt.Errorf("http 500: internal server error"), false},
		{"transient 502", fmt.Errorf("http 502: bad gateway"), false},
		{"rate limit 429", fmt.Errorf("http 429: rate limited"), false},
		{"auth 401", fmt.Errorf("http 401: unauthorized"), false},
		{"auth 403", fmt.Errorf("http 403: forbidden"), false},
		{"permanent 400", fmt.Errorf("http 400: bad request"), true},
		{"context overflow", fmt.Errorf("maximum context length exceeded"), true},
		{"keyword unauthorized", fmt.Errorf("Unauthorized access"), false},
		{"keyword api key", fmt.Errorf("invalid API key provided"), false},
		{"generic error", fmt.Errorf("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPermanentError(tt.err)
			if got != tt.permanent {
				t.Errorf("IsPermanentError(%v) = %v, want %v", tt.err, got, tt.permanent)
			}
		})
	}
}

func TestErrorCategory_CooldownMultiplier(t *testing.T) {
	tests := []struct {
		cat  ErrorCategory
		want int
	}{
		{CategoryTransient, 1},
		{CategoryRateLimit, 3},
		{CategoryAuth, 10},
		{CategoryBilling, 10},
		{CategoryContextOverflow, 1},
		{CategoryPermanent, 1},
	}
	for _, tt := range tests {
		t.Run(tt.cat.String(), func(t *testing.T) {
			if got := tt.cat.CooldownMultiplier(); got != tt.want {
				t.Errorf("%v.CooldownMultiplier() = %d, want %d", tt.cat, got, tt.want)
			}
		})
	}
}
