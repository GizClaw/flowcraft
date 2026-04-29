package llm

import (
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// TestErrorCategory_AliasIdentity locks the LLM ErrorCategory alias to
// the underlying errdefs.ProviderCategory enum and the CategoryXxx
// constants to their ProviderXxx counterparts. If the alias chain ever
// drifts (e.g. someone redefines the type instead of aliasing), this
// test will surface it before any consumer of fallback.go does at
// runtime — the categories would silently misclassify.
func TestErrorCategory_AliasIdentity(t *testing.T) {
	cases := []struct {
		name string
		got  ErrorCategory
		want errdefs.ProviderCategory
	}{
		{"transient", CategoryTransient, errdefs.ProviderTransient},
		{"rate_limit", CategoryRateLimit, errdefs.ProviderRateLimit},
		{"auth", CategoryAuth, errdefs.ProviderAuth},
		{"billing", CategoryBilling, errdefs.ProviderBilling},
		{"context_overflow", CategoryContextOverflow, errdefs.ProviderContextOverflow},
		{"permanent", CategoryPermanent, errdefs.ProviderPermanent},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: alias drift: %d != %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestCategoryString pins the metric / log label tokens. Dashboards
// filter by these strings; renaming is observable and intentional.
func TestCategoryString(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		if got := CategoryString(tc.cat); got != tc.want {
			t.Errorf("CategoryString(%d) = %q, want %q", tc.cat, got, tc.want)
		}
	}
}

// TestShouldFallback pins the chain-stop policy: ContextOverflow and
// Permanent are the only categories that must NOT advance to the next
// provider in the FallbackLLM chain (downstream sees the same input
// and fails the same way). A regression here would either degrade UX
// (give up too early) or burn quota (retry on a definite no).
func TestShouldFallback(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		t.Run(CategoryString(tc.cat), func(t *testing.T) {
			if got := ShouldFallback(tc.cat); got != tc.expect {
				t.Errorf("ShouldFallback(%v) = %v, want %v", tc.cat, got, tc.expect)
			}
		})
	}
}

// TestCooldownMultiplier pins the per-category breaker hold:
// Auth/Billing get the long penalty (credentials don't fix themselves),
// RateLimit gets a moderate hold to let the upstream window roll over.
func TestCooldownMultiplier(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		t.Run(CategoryString(tc.cat), func(t *testing.T) {
			if got := CooldownMultiplier(tc.cat); got != tc.want {
				t.Errorf("CooldownMultiplier(%v) = %d, want %d", tc.cat, got, tc.want)
			}
		})
	}
}

// TestClassifyError_DispatchesToErrdefs is a single-row smoke test
// confirming the alias still routes into errdefs.ClassifyProvider.
// The full classification surface is pinned in errdefs/http_test.go;
// this guard catches "someone replaced the body with a stub returning
// CategoryTransient unconditionally" regressions.
func TestClassifyError_DispatchesToErrdefs(t *testing.T) {
	if got := ClassifyError(fmt.Errorf("rate limit exceeded")); got != CategoryRateLimit {
		t.Errorf("ClassifyError did not dispatch to errdefs: got %d, want %d",
			got, CategoryRateLimit)
	}
}

// TestIsPermanentError covers the public convenience predicate used
// outside fallback.go (e.g. callers that want to short-circuit retry
// loops without importing the category enum). Values mirror
// ShouldFallback (which fallback.go uses internally) but the test
// exists separately because IsPermanentError is the one symbol from
// errors.go that external callers depend on.
func TestIsPermanentError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"nil", nil, false},
		{"transient 500", fmt.Errorf("http 500: internal server error"), false},
		{"rate limit 429", fmt.Errorf("http 429: rate limited"), false},
		{"auth 401", fmt.Errorf("http 401: unauthorized"), false},
		{"permanent 400", fmt.Errorf("http 400: bad request"), true},
		{"context overflow", fmt.Errorf("maximum context length exceeded"), true},
		{"keyword api key", fmt.Errorf("invalid API key provided"), false},
		{"generic error", fmt.Errorf("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPermanentError(tc.err); got != tc.permanent {
				t.Errorf("IsPermanentError(%v) = %v, want %v", tc.err, got, tc.permanent)
			}
		})
	}
}
