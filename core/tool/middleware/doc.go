// Package middleware carries the built-in cross-cutting concerns for tool
// dispatch. Each constructor returns a [tool.Middleware] — a wrapper around
// [tool.Dispatch] — and knows nothing about individual tools; the two that
// read [tool.Catalog] metadata (RateLimit, TimeoutWithCatalog) are the
// deliberate exceptions and take the catalog explicitly.
//
// # Chain order
//
// [FromSettings] is the canonical chain, outermost first, and the order is a
// contract rather than a preference:
//
//		Recover → Telemetry → ResultLimit → Truncate → Timeout → Concurrency
//
//	  - Recover must be outermost: it is what keeps a panicking tool from
//	    escaping [tool.Dispatcher.ExecuteAll]'s fan-out goroutine and taking the
//	    process down.
//	  - ResultLimit sits outside Timeout so the timeout's own error result is
//	    bounded like any other result.
//	  - Truncate sits inside ResultLimit: it persists the tool's full output and
//	    hands the limiter only its excerpt, so the spill file never holds an
//	    already-limited result.
//	  - Concurrency is innermost, so a slot is held for the shortest possible
//	    time and waiting callers do not occupy the retry/telemetry path.
//
// # Invariants
//
// A value a deployment document can reach is validated at construction and
// reported as an errdefs.Validation error; only the Must* forms panic, and
// only for values that are compile-time constants. [FromSettings] is the
// single configuration entry point, so the same checks are not repeated at
// every call site.
//
// Middleware mutate nothing about the call itself: they may shorten a result,
// refuse it, or record it, but the tool's arguments and its IsError verdict
// stay the tool's own.
//
// # Boundary
//
// Policy that depends on the tool's declaration (rate limits, self-timeout
// claims) is read from [tool.ToolMeta] here; the declarations themselves
// belong to the tools and their sources.
package middleware
