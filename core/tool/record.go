package tool

import (
	"context"
	"sync"
)

// The failure box and the active call id are diagnostics, not authority: they travel
// on the context because every middleware and every tool would otherwise have to
// thread them through. Authority travels in [Call], as a parameter.

// failureRecord is the per-call box shared by the middleware chain.
type failureRecord struct {
	mu   sync.Mutex
	kind FailureKind
}

type recordKey struct{}

type record struct {
	id      string
	failure *failureRecord
}

// WithCallID returns a context carrying id as the active tool call id, keeping the
// classification box dispatch installed.
func WithCallID(ctx context.Context, id string) context.Context {
	current, _ := ctx.Value(recordKey{}).(record)
	if current.failure == nil {
		current.failure = &failureRecord{}
	}
	current.id = id
	return context.WithValue(ctx, recordKey{}, current)
}

// CallIDFromContext returns the active tool call id. ok=false means the caller is
// outside a tool execution; consumers treat that as "unknown" rather than an error.
func CallIDFromContext(ctx context.Context) (string, bool) {
	current, ok := ctx.Value(recordKey{}).(record)
	if !ok || current.id == "" {
		return "", false
	}
	return current.id, true
}

// WithFailureRecorder installs the per-call classification box. Dispatch calls it
// once per call, before the middleware chain, so a classification is possible
// whether or not instrumentation is installed; a context that already carries one is
// returned unchanged, so an inner stage never shadows the executor's box.
func WithFailureRecorder(ctx context.Context) context.Context {
	current, _ := ctx.Value(recordKey{}).(record)
	if current.failure != nil {
		return ctx
	}
	current.failure = &failureRecord{}
	return context.WithValue(ctx, recordKey{}, current)
}

// RecordFailure classifies the current call's failure.
//
// The first classification wins: the innermost stage is closest to the cause, and an
// outer timeout wrapping an inner policy denial would otherwise rewrite the reason
// after the fact.
func RecordFailure(ctx context.Context, kind FailureKind) {
	if kind == "" {
		return
	}
	current, ok := ctx.Value(recordKey{}).(record)
	if !ok || current.failure == nil {
		return
	}
	current.failure.mu.Lock()
	if current.failure.kind == "" {
		current.failure.kind = kind
	}
	current.failure.mu.Unlock()
}

// FailureKindFrom returns the classification recorded for ctx.
func FailureKindFrom(ctx context.Context) (FailureKind, bool) {
	current, ok := ctx.Value(recordKey{}).(record)
	if !ok || current.failure == nil {
		return "", false
	}
	current.failure.mu.Lock()
	defer current.failure.mu.Unlock()
	return current.failure.kind, current.failure.kind != ""
}
