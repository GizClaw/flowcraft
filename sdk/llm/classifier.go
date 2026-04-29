package llm

// LLM-fallback decision primitives.
//
// The HTTP-status / keyword / SDK-error → errdefs translation lives in
// sdk/errdefs/http.go because it is shared by every external-provider
// boundary in the SDK (chat completion, embeddings, rerank, ...). This
// file is the LLM-domain layer on top:
//
//   - ErrorCategory aliases errdefs.ProviderCategory so the code in
//     sdk/llm/fallback.go stays in vocabulary terms its readers expect
//     (Auth / Billing / RateLimit / ContextOverflow / ...) rather than
//     having to reach across packages.
//
//   - ShouldFallback / CooldownMultiplier / CategoryString encode the
//     fallback chain's policy on top of that category enum: when to
//     give up and stop trying further providers, how long to keep a
//     tripped breaker open, and what label to emit on per-category
//     metrics.
//
//   - ClassifyError is a thin domain-named alias of
//     errdefs.ClassifyProvider; sdk/llm/fallback.go calls it on every
//     provider error to decide what to do next.
//
// External LLM providers (sdkx/llm/*) and any other SDK boundary
// (sdkx/embedding/*, future sdkx/rerank, ...) MUST call
// errdefs.ClassifyProviderError / errdefs.ClassifyHTTPStatus directly —
// they want the resulting errdefs class, not the LLM-fallback enum.
// Re-exporting those helpers here would create a phantom dependency
// from peer capabilities through sdk/llm purely for namespace reasons.

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrorCategory is an LLM-domain alias of errdefs.ProviderCategory used
// by FallbackLLM to pick cooldown durations and to decide whether to
// move on to the next provider in the chain.
type ErrorCategory = errdefs.ProviderCategory

// LLM-domain aliases for the underlying ProviderXxx constants. Kept
// under the historical Category* names so call sites in fallback.go and
// in tests need no migration when the underlying enum was hoisted into
// errdefs.
const (
	CategoryTransient       = errdefs.ProviderTransient
	CategoryRateLimit       = errdefs.ProviderRateLimit
	CategoryAuth            = errdefs.ProviderAuth
	CategoryBilling         = errdefs.ProviderBilling
	CategoryContextOverflow = errdefs.ProviderContextOverflow
	CategoryPermanent       = errdefs.ProviderPermanent
)

// CategoryString returns a stable short token suitable for log fields
// and metric labels ("rate_limit", "auth", "billing",
// "context_overflow", "permanent", "transient"). It lives as a free
// function rather than a method because ErrorCategory is a type alias —
// methods on alias types must be defined on the underlying type, and
// errdefs deliberately doesn't carry LLM-domain naming.
func CategoryString(c ErrorCategory) string {
	switch c {
	case CategoryRateLimit:
		return "rate_limit"
	case CategoryAuth:
		return "auth"
	case CategoryBilling:
		return "billing"
	case CategoryContextOverflow:
		return "context_overflow"
	case CategoryPermanent:
		return "permanent"
	default:
		return "transient"
	}
}

// ShouldFallback reports whether a given category should try the next
// provider in the FallbackLLM chain. Permanent and ContextOverflow stop
// the chain because every downstream provider will see the same input
// and fail the same way; everything else is worth trying again.
func ShouldFallback(c ErrorCategory) bool {
	switch c {
	case CategoryPermanent, CategoryContextOverflow:
		return false
	default:
		return true
	}
}

// CooldownMultiplier returns the per-category multiplier for the base
// FallbackLLM cooldown: Auth/Billing get a long penalty (the credentials
// won't fix themselves on the next call), RateLimit gets a moderate
// hold to let the upstream window roll over, everything else uses the
// configured base cooldown unchanged.
func CooldownMultiplier(c ErrorCategory) int {
	switch c {
	case CategoryAuth, CategoryBilling:
		return 10
	case CategoryRateLimit:
		return 3
	default:
		return 1
	}
}

// ClassifyError determines an ErrorCategory for fallback decisions.
// Thin domain-named alias of errdefs.ClassifyProvider so fallback.go
// reads in fallback-domain vocabulary.
func ClassifyError(err error) ErrorCategory {
	return errdefs.ClassifyProvider(err)
}
