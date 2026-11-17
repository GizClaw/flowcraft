package tool

import (
	"context"
	"sync"
)

// Call is the tool projection of one tool call: the model-issued id and the box
// the middleware chain classifies a failure in.
//
// The value is immutable; the box it points at is the one mutable thing. Only
// this package can put a box in a Call — the field is private — so a call built
// by another package always starts a fresh classification, never inheriting the
// previous call's.
type Call struct {
	// ID is the model-issued id of the call being executed.
	ID string
	// failure is the per-call classifier box shared by the middleware chain. It
	// is a pointer so inner stages can write while the outer instrumentation
	// reads, and mutex-guarded because a timeout boundary runs the call on
	// another goroutine.
	failure *failureRecord
}

// callKey is the context key for the active [Call].
type callKey struct{}

// failureRecord is the per-call box shared by the middleware chain.
type failureRecord struct {
	mu   sync.Mutex
	kind FailureKind
}

// WithCall replaces the call attached to ctx, installing a fresh classification
// box when the value carries none. The box is created here rather than by the
// telemetry middleware so every call has one whether or not instrumentation is
// installed.
func WithCall(ctx context.Context, c Call) context.Context {
	if c.failure == nil {
		c.failure = &failureRecord{}
	}
	return context.WithValue(ctx, callKey{}, c)
}

// CallFrom returns the call attached to ctx. ok=false outside a tool call, and
// also for a Call that carries neither an id nor a classification box.
func CallFrom(ctx context.Context) (Call, bool) {
	if ctx == nil {
		return Call{}, false
	}
	call, ok := ctx.Value(callKey{}).(Call)
	if !ok || (call.ID == "" && call.failure == nil) {
		return Call{}, false
	}
	return call, true
}

// WithCallID returns a derived context carrying id as the active tool call id.
// The call already on ctx keeps its classification box: the executor installed
// it before the middleware chain, and replacing it here would drop a
// classification an inner stage wrote.
//
// Deprecated: use [WithCall].
func WithCallID(ctx context.Context, id string) context.Context {
	call, ok := CallFrom(ctx)
	if !ok {
		return WithCall(ctx, Call{ID: id})
	}
	call.ID = id
	return WithCall(ctx, call)
}

// CallIDFromContext returns the active tool call id carried by ctx. ok=false
// means the caller is outside a tool execution (or the id was empty); consumers
// should treat the zero value as "unknown" rather than an error.
//
// Deprecated: use [CallFrom].
func CallIDFromContext(ctx context.Context) (string, bool) {
	call, ok := CallFrom(ctx)
	if !ok || call.ID == "" {
		return "", false
	}
	return call.ID, true
}

// WithFailureRecorder returns a context that collects the failure classification
// of the call being dispatched. A call that already carries a box is returned
// unchanged, so instrumentation never shadows the executor's box; without a call
// it installs one.
//
// Deprecated: use [WithCall]; the executor installs the box for every call.
func WithFailureRecorder(ctx context.Context) context.Context {
	call, _ := CallFrom(ctx)
	if call.failure != nil {
		return ctx
	}
	return WithCall(ctx, call)
}

// RecordFailure classifies the current call's failure.
//
// The first classification wins: the innermost stage is closest to the cause,
// and an outer timeout wrapping an inner policy denial would otherwise rewrite
// the reason after the fact.
func RecordFailure(ctx context.Context, kind FailureKind) {
	if kind == "" {
		return
	}
	call, ok := CallFrom(ctx)
	if !ok || call.failure == nil {
		return
	}
	call.failure.mu.Lock()
	if call.failure.kind == "" {
		call.failure.kind = kind
	}
	call.failure.mu.Unlock()
}

// FailureKindFrom returns the classification recorded for ctx.
func FailureKindFrom(ctx context.Context) (FailureKind, bool) {
	call, ok := CallFrom(ctx)
	if !ok || call.failure == nil {
		return "", false
	}
	call.failure.mu.Lock()
	defer call.failure.mu.Unlock()
	return call.failure.kind, call.failure.kind != ""
}
