package tool

import (
	"context"
	"errors"
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
