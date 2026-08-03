package memory

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrorKind classifies a memory error for stable cross-impl handling
// by callers, telemetry, and the future agent diagnostics surface.
// The set is intentionally narrow and aligned with sdk/inference so
// shared tools can reason about both runtimes uniformly.
//
// Each kind maps to one errdefs classification: see [ErrorKind.classify].
// Callers can rely on either Kind for fine-grained checks or the
// errdefs predicates (errdefs.IsValidation, errdefs.IsInterrupted, …)
// for cross-runtime handling.
type ErrorKind string

const (
	// KindInvalidRequest means the request shape itself is
	// wrong (e.g. empty Records, malformed Scope).
	KindInvalidRequest ErrorKind = "invalid_request"
	// KindUnsupportedFeature means an Impl refuses a feature
	// the request asked for. Carries a Rejected Decision.
	KindUnsupportedFeature ErrorKind = "unsupported_feature"
	// KindInvalidExtension means hook settings contained an
	// unknown or conflicting key.
	KindInvalidExtension ErrorKind = "invalid_extension"
	// KindScopeInvalid means Scope.Validate failed.
	KindScopeInvalid ErrorKind = "scope_invalid"
	// KindNotConfigured means an op was called without a
	// registered Impl.
	KindNotConfigured ErrorKind = "not_configured"
	// KindPolicyDenied means hook settings forbid the op
	// (e.g. recall on a disabled dataset).
	KindPolicyDenied ErrorKind = "policy_denied"
	// KindOperationInterrupted means the caller cancelled the
	// call via context.
	KindOperationInterrupted ErrorKind = "operation_interrupted"
	// KindProviderFailure means the Impl-side transport or
	// persistence failed.
	KindProviderFailure ErrorKind = "provider_failure"
	// KindInternal means a programming error (compile ledger
	// gap, nil deref, etc.) — never returned for a request
	// shape problem the caller could have caught.
	KindInternal ErrorKind = "internal"
)

// Error is the typed error returned by every memory operation. The
// Kind is stable; Op and Field route the error in diagnostics; the
// wrapped cause carries the original error from the underlying
// layer (impl, store, transport) and is exposed through Unwrap so
// errors.Is / errors.As can walk the chain.
//
// The cause is intentionally unexported: the runtime does not want
// its Error() to leak prompts, credentials, or wire payloads that
// the cause may carry. Use Unwrap or errors.Is to inspect the cause.
type Error struct {
	Kind  ErrorKind
	Op    Operation
	Field FieldID
	cause error
}

// Error renders a stable, log-friendly message. The cause is not
// included; use Unwrap to walk the chain.
func (e *Error) Error() string {
	switch {
	case e.Field != "":
		return fmt.Sprintf("memory: %s op=%s field=%s", e.Kind, e.Op, e.Field)
	case e.Op != "":
		return fmt.Sprintf("memory: %s op=%s", e.Kind, e.Op)
	default:
		return fmt.Sprintf("memory: %s", e.Kind)
	}
}

// Unwrap exposes the underlying cause for errors.Is / errors.As.
// The cause is the errdefs-classified wrapper from newError; callers
// who need the original (un-classified) error should keep walking
// the chain.
func (e *Error) Unwrap() error { return e.cause }

// newError builds an Error with the cause classified under the
// errdefs taxonomy that matches its Kind. The classified wrapper
// is what Unwrap returns, so callers can use errdefs.IsValidation,
// errdefs.IsInterrupted, errdefs.IsNotAvailable, errdefs.IsPolicyDenied,
// errdefs.IsInternal, etc. on the returned *Error.
func newError(kind ErrorKind, op Operation, field FieldID, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(kind))
	}
	return &Error{
		Kind:  kind,
		Op:    op,
		Field: field,
		cause: kind.classify(cause),
	}
}

// classify maps a memory ErrorKind to the corresponding errdefs
// classification. The returned error wraps cause with the matching
// errdefs helper so predicates like errdefs.IsValidation can
// recognise the kind.
//
// Kinds without a direct errdefs twin (KindScopeInvalid,
// KindNotConfigured, KindUnsupportedFeature, KindInvalidExtension)
// fall back to the closest errdefs category; the Kind field stays
// authoritative for memory-specific routing.
func (kind ErrorKind) classify(cause error) error {
	switch kind {
	case KindInvalidRequest, KindScopeInvalid,
		KindUnsupportedFeature, KindInvalidExtension:
		return errdefs.Validation(cause)
	case KindNotConfigured:
		return errdefs.NotAvailable(cause)
	case KindPolicyDenied:
		return errdefs.PolicyDenied(cause)
	case KindOperationInterrupted:
		// Honour a pre-classified cause (e.g. from a context
		// cancellation that already wrapped a Timeout/Aborted);
		// otherwise default to Interrupted.
		if classified := errdefs.FromContext(cause); errdefs.HasClassification(classified) {
			if errdefs.IsTimeout(classified) || errdefs.IsAborted(classified) {
				return classified
			}
		}
		return errdefs.Interrupted(cause)
	case KindProviderFailure:
		// Pass through a pre-classified cause so a transport
		// timeout stays a Timeout, a 404 stays a NotFound, etc.
		if classified := errdefs.FromContext(cause); errdefs.HasClassification(classified) {
			return classified
		}
		return errdefs.NotAvailable(cause)
	case KindInternal:
		return errdefs.Internal(cause)
	default:
		return errdefs.Internal(fmt.Errorf("memory: unknown error kind %q: %w", kind, cause))
	}
}

// AsError extracts a *Error from any returned error. Returns nil
// when err is nil or is not a memory error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// IsKind reports whether err is (or wraps) a *Error whose Kind
// matches want. Use it for memory-specific routing; for
// cross-runtime checks, prefer the errdefs predicates.
func IsKind(err error, want ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == want
}
