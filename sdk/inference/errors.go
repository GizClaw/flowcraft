package inference

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type ErrorKind string

const (
	InvalidRequest            ErrorKind = "invalid_request"
	UnsupportedOperation      ErrorKind = "unsupported_operation"
	UnsupportedFeature        ErrorKind = "unsupported_feature"
	InvalidExtension          ErrorKind = "invalid_extension"
	UnknownProvider           ErrorKind = "unknown_provider"
	UnknownModel              ErrorKind = "unknown_model"
	UnknownProfile            ErrorKind = "unknown_profile"
	PolicyDenied              ErrorKind = "policy_denied"
	OperationInterrupted      ErrorKind = "operation_interrupted"
	CompilerContractViolation ErrorKind = "compiler_contract_violation"
	ProviderFailure           ErrorKind = "provider_failure"
	InvalidProviderResponse   ErrorKind = "invalid_provider_response"
)

// Error carries safe structural context. Error deliberately excludes the
// underlying cause, which may contain prompts, credentials, or wire payloads.
type Error struct {
	Kind      ErrorKind
	Operation Operation
	Field     FieldID
	cause     error
}

func NewError(kind ErrorKind, operation Operation, field FieldID, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(kind))
	}
	return &Error{
		Kind:      kind,
		Operation: operation,
		Field:     field,
		cause:     kind.classify(cause),
	}
}

func newProviderError(
	operation Operation,
	provider string,
	cause error,
) *Error {
	classified := errdefs.FromContext(cause)
	if !errdefs.HasClassification(classified) {
		classified = errdefs.ClassifyProviderError(provider, classified)
	}
	return NewError(ProviderFailure, operation, "", classified)
}

func (e *Error) Error() string {
	message := string(e.Kind)
	if e.Operation != "" {
		message += " during " + string(e.Operation)
	}
	if e.Field != "" {
		message += " at " + string(e.Field)
	}
	return message
}

func (e *Error) Unwrap() error { return e.cause }

func IsKind(err error, kind ErrorKind) bool {
	var inferenceErr *Error
	return errors.As(err, &inferenceErr) && inferenceErr.Kind == kind
}

func (kind ErrorKind) isCompilerRejection() bool {
	switch kind {
	case UnsupportedFeature, InvalidExtension:
		return true
	default:
		return false
	}
}

func (kind ErrorKind) classify(cause error) error {
	switch kind {
	case InvalidRequest, UnsupportedFeature, InvalidExtension:
		return errdefs.Validation(cause)
	case UnknownProvider, UnknownModel, UnknownProfile:
		return errdefs.NotFound(cause)
	case UnsupportedOperation:
		return errdefs.NotAvailable(cause)
	case PolicyDenied:
		return errdefs.PolicyDenied(cause)
	case OperationInterrupted:
		classified := errdefs.FromContext(cause)
		if errdefs.IsAborted(classified) || errdefs.IsTimeout(classified) {
			return classified
		}
		return errdefs.Aborted(classified)
	case CompilerContractViolation, InvalidProviderResponse:
		return errdefs.Internal(cause)
	case ProviderFailure:
		classified := errdefs.FromContext(cause)
		if errdefs.HasClassification(classified) {
			return classified
		}
		return errdefs.NotAvailable(classified)
	default:
		return errdefs.Internal(fmt.Errorf("unknown inference error kind %q: %w", kind, cause))
	}
}
