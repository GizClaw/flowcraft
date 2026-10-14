package inference

import (
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
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
	// ProviderTruncated marks a stream that ended before the provider
	// emitted a terminal finish event. It is distinct from
	// InvalidProviderResponse so retry/degradation policies can treat it
	// as transient (the request may simply have been cut off mid-flight)
	// instead of permanent response corruption.
	// Contract: core never auto-retries this error. Its NotAvailable/503
	// classification means "this attempt is void and worth re-initiating
	// at caller granularity" — partial content may already have been
	// committed by the inference node before the error propagated, so an
	// internal mid-stream retry would duplicate output.
	ProviderTruncated ErrorKind = "provider_truncated"
	// ProviderResponseUnusable marks a response that completed — a
	// terminal finish event was received, or a unary call returned — but
	// whose assembled payload violates the request contract: empty
	// completed content, missing or unrequested parts, invalid tool-call
	// structure, tool-choice violations, or usage and structure rules.
	// It is distinct from InvalidProviderResponse, which stays for
	// payloads that could not be decoded or assembled at all (wire-level
	// corruption), so callers can treat this attempt as void and
	// re-initiate at their own granularity while corruption remains
	// non-retryable.
	// Contract: core never auto-retries this error. Its NotAvailable/503
	// classification means "this attempt is void and worth re-initiating
	// at caller granularity" — partial content may already have been
	// committed by the inference node before the error propagated, so an
	// internal retry would duplicate output.
	ProviderResponseUnusable ErrorKind = "provider_response_unusable"
	// UndefinedTool marks a response whose tool calls name tools absent
	// from the request's definitions. It is distinct from
	// InvalidProviderResponse so callers can offer a recoverable path
	// (e.g. feedback that sends the model back to tool_search) without
	// weakening the contract for genuinely corrupt responses.
	UndefinedTool ErrorKind = "undefined_tool"
)

// Error carries safe structural context. Error() deliberately excludes the
// underlying cause, so its text is safe for routine logs and API responses.
//
// Unwrap retains the cause for errors.Is/errors.As and errdefs classification.
// The error chain is therefore diagnostic data, not redacted output: callers
// must not serialize or log unwrapped causes where prompts, credentials, or
// provider wire payloads could be exposed.
type Error struct {
	Kind      ErrorKind
	Operation Operation
	Field     FieldID
	// Detail is a stable, redacted stage label identifying where within
	// the operation the failure was detected (for example
	// "stream.finish.missing" for a stream that ended without a finish
	// reason). Unlike cause it never carries provider wire payloads,
	// prompts, or raw response fragments, so it is safe for Error()
	// text, routine logs, and API responses.
	Detail string
	// RetryAfter is a server-provided backoff hint (Retry-After) when a
	// provider failure carries one. Zero means no hint. It is diagnostic
	// metadata, not part of the redacted Error() text.
	RetryAfter time.Duration
	// WireAttempts is the number of HTTP sends the provider transport made
	// before surfacing this failure. Zero means the provider did not report
	// a count.
	WireAttempts int
	// RequestID is the provider-assigned request identifier attached to
	// this failure when the provider reported one. Empty otherwise.
	// Stream failures that end early may carry the provider's response
	// identifier (chat/message id) instead when the transport-level request
	// id is unknown, so operators can still correlate the truncated
	// request with the provider.
	// Runtime telemetry mirrors it onto error spans and logs as
	// llm.request.id.
	RequestID string
	// UndefinedToolCall is the first tool call rejected because its name
	// was absent from the request's definitions. Populated only for
	// UndefinedTool errors; a recovering caller uses it to reconstruct the
	// call for the stored recovery feedback.
	UndefinedToolCall *message.ToolCall
	cause             error
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
	err := NewError(ProviderFailure, operation, "", classified)
	if retryAfter, ok := errdefs.RetryAfter(cause); ok {
		err.RetryAfter = retryAfter
	}
	err.WireAttempts = errdefs.RetryCount(cause)
	if requestID, ok := errdefs.RequestID(cause); ok {
		err.RequestID = requestID
	}
	return err
}

func (e *Error) Error() string {
	message := string(e.Kind)
	if e.Operation != "" {
		message += " during " + string(e.Operation)
	}
	if e.Field != "" {
		message += " at " + string(e.Field)
	}
	if e.Detail != "" {
		message += ": " + e.Detail
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
	case ProviderTruncated, ProviderResponseUnusable:
		return errdefs.NotAvailable(cause)
	case UndefinedTool:
		// Deterministic response violation: the model referenced a tool
		// it was never shown. Retrying the same request cannot help, so
		// it classifies as validation (non-retryable) rather than
		// internal.
		return errdefs.Validation(cause)
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

// newResponseValidationError classifies a GenerateResponse.ValidateFor
// failure. Undefined tool calls become UndefinedTool (deterministic,
// potentially recoverable); every other contract violation becomes
// ProviderResponseUnusable — a completed response whose payload the request
// contract rejects — while InvalidProviderResponse stays reserved for
// wire-level corruption raised on the decode and assembly paths.
// scope identifies the call phase independently of the operation.
func newResponseValidationError(operation Operation, scope string, err error) *Error {
	out := NewError(ProviderResponseUnusable, operation, "", err)
	var ute *undefinedToolError
	if errors.As(err, &ute) {
		out = NewError(UndefinedTool, operation, "", err)
		call := ute.Call
		out.UndefinedToolCall = &call
	}
	var check *generateResponseCheckError
	if errors.As(err, &check) {
		prefix := scope + ".validation."
		if scope == "stream.finish" {
			// Preserve the terminal labels already used by logs and alerts.
			switch check.check {
			case generateResponseCheckToolCall, generateResponseCheckMismatch, generateResponseCheckUndefinedTool:
				prefix = scope + "."
			}
		}
		out.Detail = prefix + string(check.check)
	}
	return out
}

// redactedCause is implemented by inference-owned error types whose message
// is built from flowcraft-generated text — our format strings plus
// identifier-like tokens (ids, names, media types, counts, JSON types) —
// never provider wire payloads, prompts, or raw response fragments. It
// marks the only causes CauseMessage may surface.
type redactedCause interface {
	error
	redactedCause()
}

// CauseMessage returns the message of the first inference-owned cause
// beneath the outer *Error in err's chain. It returns "" when the chain
// carries no *Error, or when none of its causes is marked
// redacted-by-construction: driver, transport, and context errors stay out,
// because Error documents the chain as diagnostic data rather than redacted
// output.
//
// It exists so routine logs can carry the actual reason behind a stable
// Detail label — for example the terminal check that rejected a completed
// response — without serializing an arbitrary error chain.
func CauseMessage(err error) string {
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		return ""
	}
	for cause := inferenceErr.cause; cause != nil; cause = errors.Unwrap(cause) {
		if marked, ok := cause.(redactedCause); ok {
			return marked.Error()
		}
	}
	return ""
}
