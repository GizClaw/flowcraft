// Package fleet hosts the live runtime: it converts a resolver.Plan
// into N running vessel.Captain instances, owns the shared
// dependencies (event bus per vessel, daemon-wide rate limiters),
// and exposes the routing API the HTTP layer calls.
//
// Why a separate package from `runtime`: the fleet has to be
// importable by tests that want to drive Submit / Drain / Stop
// directly without spinning up a TCP listener. Keeping the HTTP
// surface in `api` and the bind / signal / log wiring in `runtime`
// means each layer has one reason to change.
//
// Concurrency model:
//
//   - Each Captain runs in its own goroutine subtree. The fleet
//     never holds a Captain lock; it dispatches via the public
//     Submit / Drain / Stop methods.
//   - The fleet's own mutex protects only the captain map (so
//     Submit / List / Stop can race safely).
//   - A daemon-wide concurrency semaphore wraps every Submit when
//     DaemonPlan.MaxConcurrentRuns > 0.
//   - Per-LLMProfile rate limiters live in this package and apply
//     across vessels — engine factories pull them from a context
//     value injected before each dispatch.
package fleet
