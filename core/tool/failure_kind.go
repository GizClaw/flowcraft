package tool

import (
	"context"
	"errors"
	"sync"
)

// FailureKind classifies why one tool call failed.
//
// The set is closed on purpose: dashboards group failures by this value, so
// adding one is a vocabulary change rather than a free-form string. A failure
// that no producer classified is reported by instrumentation as "unclassified",
// which is a signal that a short-circuit is missing a call to [RecordFailure].
type FailureKind string

const (
	// FailureNotFound means the catalog has no tool by that name.
	FailureNotFound FailureKind = "not_found"
	// FailurePolicyDenied means a policy short-circuited the call before the
	// tool ran: an approval was refused, or a host rule rejected it.
	FailurePolicyDenied FailureKind = "policy_denied"
	// FailureTimeout means the call exceeded its deadline.
	FailureTimeout FailureKind = "timeout"
	// FailureCanceled means the caller's context ended the call: the run was
	// stopped, which is neither the tool's failure nor a deadline.
	FailureCanceled FailureKind = "canceled"
	// FailurePanicked means the tool, or middleware inside it, panicked and was
	// recovered.
	FailurePanicked FailureKind = "panicked"
	// FailureToolError means the tool ran and returned an error.
	FailureToolError FailureKind = "tool_error"
)

// classifyToolFailure maps the error a tool returned onto the vocabulary. A tool
// that respects its context reports the deadline or cancellation the caller
// imposed; that is not a tool bug, and calling it one would bury real tool
// failures in the same bucket.
func classifyToolFailure(err error) FailureKind {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	default:
		return FailureToolError
	}
}

// failureKey is the context key for the per-call failure record.
type failureKey struct{}

// failureRecord is the per-call box shared by the middleware chain. It is a
// pointer so inner stages can write while the outer instrumentation reads, and
// mutex-guarded because a timeout boundary runs the call on another goroutine.
type failureRecord struct {
	mu   sync.Mutex
	kind FailureKind
}

// WithFailureRecorder returns a context that collects the failure classification
// of the call being dispatched.
//
// Instrumentation wraps the chain with it (which is why it is called by the
// telemetry middleware, not by the executor); stages deeper in the chain call
// [RecordFailure] on the context they were handed. Without a recorder both
// calls are cheap no-ops, so a hand-built chain keeps working.
func WithFailureRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, failureKey{}, &failureRecord{})
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
	record, ok := ctx.Value(failureKey{}).(*failureRecord)
	if !ok {
		return
	}
	record.mu.Lock()
	if record.kind == "" {
		record.kind = kind
	}
	record.mu.Unlock()
}

// FailureKindFrom returns the classification recorded for ctx.
func FailureKindFrom(ctx context.Context) (FailureKind, bool) {
	if ctx == nil {
		return "", false
	}
	record, ok := ctx.Value(failureKey{}).(*failureRecord)
	if !ok {
		return "", false
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.kind, record.kind != ""
}
