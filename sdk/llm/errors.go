package llm

import "errors"

// Sentinel errors for FallbackLLM failure modes.
// Use errors.Is to distinguish between "all breakers open" and "all providers tried and failed".
var (
	ErrAllProvidersOpen   = errors.New("llm: all providers unavailable (circuit breaker open)")
	ErrAllProvidersFailed = errors.New("llm: all providers failed")
)

// IsPermanentError reports whether err represents a permanent failure that
// should not trigger fallback. Delegates to ClassifyError internally.
func IsPermanentError(err error) bool {
	return !ClassifyError(err).ShouldFallback()
}
