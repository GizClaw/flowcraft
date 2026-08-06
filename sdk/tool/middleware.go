package tool

import (
	"slices"
)

// Middleware decorates a Dispatch, returning a new Dispatch that may
// run code before/after the wrapped call (audit, approval, rate-limit,
// secret-resolve, retry, etc.). Middlewares are composed in
// outermost-first order: the first registered middleware sees the
// call first and the result last.
//
// A middleware MUST forward to next unless it intentionally
// short-circuits (e.g. policy denial). Short-circuit responses should
// set Result.IsError=true and put a human-readable reason in
// Content; classify the underlying error via sdk/errdefs where
// possible (PolicyDenied, BudgetExceeded, RateLimit).
type Middleware func(next Dispatch) Dispatch

// composeDispatch wraps core in the given middlewares, outermost
// first. With chain [a, b] and core c, the resulting Dispatch is
// a(b(c)) — i.e. a is invoked first. nil middleware values are
// silently skipped to keep callers ergonomic when conditionally
// adding hooks.
func composeDispatch(core Dispatch, mws []Middleware) Dispatch {
	for _, mw := range slices.Backward(mws) {
		if mw == nil {
			continue
		}
		core = mw(core)
	}
	return core
}
