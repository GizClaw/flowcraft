package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// Dispatch is the function form of "execute one tool call and return
// its result". The Registry's own Execute method is itself a Dispatch
// (modulo the receiver). Middleware operates on this signature.
//
// Implementations should treat the input call as immutable and must
// always return a ToolResult — errors are reported via
// ToolResult.IsError, never returned out-of-band.
type Dispatch func(ctx context.Context, call model.ToolCall) model.ToolResult

// Middleware decorates a Dispatch, returning a new Dispatch that may
// run code before/after the wrapped call (audit, approval, rate-limit,
// secret-resolve, retry, etc.). Middlewares are composed in
// outermost-first order: the first registered middleware sees the
// call first and the result last.
//
// A middleware MUST forward to next unless it intentionally
// short-circuits (e.g. policy denial). Short-circuit responses should
// set ToolResult.IsError=true and put a human-readable reason in
// Content; classify the underlying error via sdk/errdefs where
// possible (PolicyDenied, BudgetExceeded, RateLimit).
type Middleware func(next Dispatch) Dispatch

// Use appends middleware to the Registry's dispatch chain. Middlewares
// are applied in registration order: Use(a, b) means a runs outermost,
// then b, then the core dispatch. nil middleware values are silently
// skipped to keep callers ergonomic when conditionally adding hooks.
func (r *Registry) Use(mws ...Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, mw := range mws {
		if mw == nil {
			continue
		}
		r.middlewares = append(r.middlewares, mw)
	}
}

// snapshotMiddlewares returns a copy of the current middleware chain
// under read-lock. The copy guards against mutation during a long
// Execute call when a concurrent Use appends another middleware.
func (r *Registry) snapshotMiddlewares() []Middleware {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.middlewares) == 0 {
		return nil
	}
	out := make([]Middleware, len(r.middlewares))
	copy(out, r.middlewares)
	return out
}

// composeDispatch wraps core in the registered middlewares, outermost
// first. With chain [a, b] and core c, the resulting Dispatch is
// a(b(c)) — i.e. a is invoked first.
func composeDispatch(core Dispatch, mws []Middleware) Dispatch {
	for i := len(mws) - 1; i >= 0; i-- {
		core = mws[i](core)
	}
	return core
}
