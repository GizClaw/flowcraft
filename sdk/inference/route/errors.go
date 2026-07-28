package route

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

type ErrorKind string

const (
	InvalidRequest            ErrorKind = "invalid_request"
	SelectorUnavailable       ErrorKind = "selector_unavailable"
	NoRoute                   ErrorKind = "no_route"
	SelectionFailed           ErrorKind = "selection_failed"
	SelectorContractViolation ErrorKind = "selector_contract_violation"
	FallbackFailed            ErrorKind = "fallback_failed"
	FallbackContractViolation ErrorKind = "fallback_contract_violation"
	FallbackLimitExceeded     ErrorKind = "fallback_limit_exceeded"
)

// Error carries safe route context without exposing selector inputs or
// implementation details.
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
	case SelectionFailed, FallbackFailed:
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
