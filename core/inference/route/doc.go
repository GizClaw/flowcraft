// Package route provides optional model selection above inference.Runtime.
//
// It owns deployment-defined tiers, normalized model scores, operation-specific
// target pools, selector contracts, and route traces. It does not declare model
// capabilities: selectors return an exact inference.ModelRef, and the provider
// compiler remains the authority on whether the concrete request is executable.
//
// Router also owns resilience: same-target retry/backoff (RetryPolicy),
// cross-target fallback, and an optional per-target circuit breaker
// (CircuitBreakerConfig). Retry defaults are conservative — only transient
// ProviderFailure without observable output is retried — and stream/session
// operations never reopen after a successful open.
// ResetCircuitBreaker clears all breaker state on the instance.
package route
