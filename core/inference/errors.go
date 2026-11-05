package inference

import (
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// ErrorKind classifies an inference failure. Callers branch on the kind, not
// on the message text: the message is for humans and may change, the kind is
// part of the contract and maps onto the shared [errdefs] categories.
type ErrorKind string

const (
	// InvalidRequest means the canonical request is malformed. It is
	// detected before any provider I/O.
	InvalidRequest ErrorKind = "invalid_request"
	// UnsupportedOperation means the model does not serve this operation
	// at all (an embedding model asked to generate).
	UnsupportedOperation ErrorKind = "unsupported_operation"
	// UnsupportedFeature means the model serves the operation but cannot
	// honor a requested field. The field is named on the error.
	UnsupportedFeature ErrorKind = "unsupported_feature"
	// InvalidExtension means a provider-specific extension failed to
	// decode or validate.
	InvalidExtension ErrorKind = "invalid_extension"
	// UnknownModel means the requested model is not declared.
	UnknownModel ErrorKind = "unknown_model"
	// PolicyDenied means a host policy refused the request before I/O.
	PolicyDenied ErrorKind = "policy_denied"
	// OperationInterrupted means the caller's context ended the call.
	OperationInterrupted ErrorKind = "operation_interrupted"
	// CompilerContractViolation means a driver compiled a different set of
	// fields than the request declared active. It is a driver bug, not a
	// caller error.
	CompilerContractViolation ErrorKind = "compiler_contract_violation"
	// ProviderFailure means the provider call itself failed.
	ProviderFailure ErrorKind = "provider_failure"
	// InvalidProviderResponse means the wire response could not be decoded.
	InvalidProviderResponse ErrorKind = "invalid_provider_response"
	// ProviderTruncated means a stream ended before a terminal finish
	// event. Partial content may already have been delivered, so core never
	// retries it internally.
	ProviderTruncated ErrorKind = "provider_truncated"
	// ProviderResponseUnusable means the call completed but its payload
	// cannot be used: empty content, a finish reason the contract does not
	// define, and so on.
	ProviderResponseUnusable ErrorKind = "provider_response_unusable"
	// UndefinedTool means the model called a tool that the request never
	// defined. It is deterministic: retrying the same request cannot help.
	UndefinedTool ErrorKind = "undefined_tool"
)

// Error is the single failure type the inference contract produces. Kind,
// Operation and Field are structural; Detail is a stable stage label safe to
// log and return to callers. The wrapped cause may carry provider payloads and
// must not be surfaced where prompts or credentials could leak.
type Error struct {
	Kind  ErrorKind
	Field Field
	// Detail is a stable, redacted stage label identifying where inside the
	// operation the failure was detected ("stream.finish.missing"). It never
	// carries prompts, credentials or wire payloads.
	Detail string
	// RetryAfter is a server-provided backoff hint. Zero means none.
	RetryAfter time.Duration
	// RequestID is the provider-assigned request identifier when the
	// provider reported one.
	RequestID string
	// UndefinedToolCall is the first call rejected by name. Populated only
	// for UndefinedTool, so a recovering caller can rebuild the call.
	UndefinedToolCall *message.ToolCall

	cause error
}

// NewError builds an inference error and classifies its cause for the shared
// error categories.
func NewError(kind ErrorKind, field Field, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(kind))
	}
	return &Error{
		Kind:  kind,
		Field: field,
		cause: kind.classify(cause),
	}
}

// WithDetail returns e with the stage label set. It mutates e and returns it so
// construction can stay flat.
func (e *Error) WithDetail(detail string) *Error {
	e.Detail = detail
	return e
}

// Error renders the redacted form: no cause text, no payload.
func (e *Error) Error() string {
	text := string(e.Kind)
	if e.Field != "" {
		text += " (" + string(e.Field) + ")"
	}
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	return text
}

// Unwrap exposes the classified cause.
func (e *Error) Unwrap() error { return e.cause }

// IsKind reports whether err is an inference error of the given kind.
func IsKind(err error, kind ErrorKind) bool {
	var inferenceErr *Error
	return errors.As(err, &inferenceErr) && inferenceErr.Kind == kind
}

func (kind ErrorKind) classify(cause error) error {
	switch kind {
	case InvalidRequest, UnsupportedFeature, InvalidExtension, UndefinedTool:
		return errdefs.Validation(cause)
	case UnknownModel:
		return errdefs.NotFound(cause)
	case UnsupportedOperation, ProviderTruncated, ProviderResponseUnusable:
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

// newUndefinedToolError builds the deterministic rejection raised when a model
// calls a tool the request never defined.
func newUndefinedToolError(call message.ToolCall) *Error {
	err := NewError(
		UndefinedTool,
		FieldTools,
		fmt.Errorf("model called undefined tool %q", call.Name),
	)
	err.UndefinedToolCall = &call
	return err
}
