package route

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

type ErrorKind string

// SelectionFailed also wraps target inspection failures: a selector-chosen
// target that the runtime cannot resolve is a routing failure, never a
// contract violation. Contract violations are reserved for selector or
// fallback outputs that break the declared Decision/Fallback contract.
const (
	InvalidRequest            ErrorKind = "invalid_request"
	SelectorUnavailable       ErrorKind = "selector_unavailable"
	NoRoute                   ErrorKind = "no_route"
	SelectionFailed           ErrorKind = "selection_failed"
	SelectorContractViolation ErrorKind = "selector_contract_violation"
	FallbackFailed            ErrorKind = "fallback_failed"
	FallbackContractViolation ErrorKind = "fallback_contract_violation"
	FallbackLimitExceeded     ErrorKind = "fallback_limit_exceeded"
	CircuitOpen               ErrorKind = "circuit_open"
)

// Error carries safe route context. Error() excludes selector inputs and
// implementation details and is safe for routine logs and API responses.
// Unwrap retains the diagnostic cause for errors.Is/errors.As and errdefs
// classification; callers must treat that error chain as potentially
// sensitive rather than as redacted output.
type Error struct {
	Kind      ErrorKind
	Operation inference.Operation
	cause     error
}

func NewError(kind ErrorKind, operation inference.Operation, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(kind))
	}
	return &Error{
		Kind:      kind,
		Operation: operation,
		cause:     classify(kind, cause),
	}
}

func (e *Error) Error() string {
	message := string(e.Kind)
	if e.Operation != "" {
		message += " during " + string(e.Operation)
	}
	return message
}

func (e *Error) Unwrap() error { return e.cause }

func IsKind(err error, kind ErrorKind) bool {
	var routeErr *Error
	return errors.As(err, &routeErr) && routeErr.Kind == kind
}

func classify(kind ErrorKind, cause error) error {
	switch kind {
	case InvalidRequest:
		return errdefs.Validation(cause)
	case SelectorUnavailable, NoRoute:
		return errdefs.NotAvailable(cause)
	case SelectionFailed, FallbackFailed, CircuitOpen:
		classified := errdefs.FromContext(cause)
		if errdefs.HasClassification(classified) {
			return classified
		}
		return errdefs.NotAvailable(classified)
	case SelectorContractViolation, FallbackContractViolation, FallbackLimitExceeded:
		return errdefs.Internal(cause)
	default:
		return errdefs.Internal(cause)
	}
}

// fallbackEligibleKind reports whether a failed inference attempt is
// transport-safe to retry on another exact target. Only local compiler
// rejections qualify: the request never reached the provider, so no provider
// side effects or partial outputs are possible.
func fallbackEligibleKind(kind inference.ErrorKind) bool {
	switch kind {
	case inference.UnsupportedOperation,
		inference.UnsupportedFeature,
		inference.InvalidExtension:
		return true
	default:
		return false
	}
}
