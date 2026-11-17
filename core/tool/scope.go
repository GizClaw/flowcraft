package tool

import "context"

// Scope is the tool projection of one run: the recorder every dispatched call
// reports to, which an assembly narrows from a [CatalogView] to a
// [CallRecorder]. It belongs to the run, so a tool call reuses the scope it was
// handed instead of attaching a projection of its own.
type Scope struct {
	// Recorder receives the calls the run dispatches. With dynamic injection it
	// has to be the run's engine.CatalogView: that is what engine.CatalogViewFrom
	// narrows, and what tool_search, the discovery pool and the model node's
	// toolset read. A host may attach any other [CallRecorder] — the domain still
	// works and calls are still recorded — but the view readers then find nothing,
	// so a dynamic assembly needs the view.
	Recorder CallRecorder
}

// scopeKey is the context key for the run's [Scope]. One key per domain: the
// view an assembly narrows travels in the Recorder field, not in a key of its
// own.
type scopeKey struct{}

// WithScope replaces the scope attached to ctx. An empty scope is ignored rather
// than attached: it would hide a projection the caller already carried, and
// "no recorder" already means absent to every reader (see [ScopeFrom]).
func WithScope(ctx context.Context, s Scope) context.Context {
	if s.Recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the scope attached to ctx, if any. A scope that carries no
// recorder reports absent, so "attached but empty" cannot shadow the projection a
// caller is about to attach: the zero Scope is what an absent domain looks like.
func ScopeFrom(ctx context.Context) (Scope, bool) {
	if ctx == nil {
		return Scope{}, false
	}
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	if !ok || scope.Recorder == nil {
		return Scope{}, false
	}
	return scope, true
}

// EnsureScope attaches rec only when the context carries no usable scope yet, so
// a caller that already has the run's projection reuses it instead of shadowing
// it. An empty Scope counts as no scope (see [ScopeFrom]), and a nil recorder
// returns ctx unchanged.
func EnsureScope(ctx context.Context, rec CallRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	if _, ok := ScopeFrom(ctx); ok {
		return ctx
	}
	return WithScope(ctx, Scope{Recorder: rec})
}
