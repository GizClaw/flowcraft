package protocol

import (
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// KindOf maps an error to the wire kind a plugin reports it as, so the
// classes craft already reasons about cross the socket with their
// meaning intact.
func KindOf(err error) CallError_Kind {
	switch {
	case err == nil:
		return CallError_UNKNOWN
	case errdefs.IsValidation(err):
		return CallError_INVALID_ARGUMENT
	case errdefs.IsNotFound(err):
		return CallError_NOT_FOUND
	case errdefs.IsUnauthorized(err), errdefs.IsForbidden(err),
		errdefs.IsPolicyDenied(err):
		return CallError_PERMISSION_DENIED
	case errdefs.IsConflict(err), errdefs.IsAborted(err):
		return CallError_FAILED_PRECONDITION
	case errdefs.IsTimeout(err):
		return CallError_TIMEOUT
	case errdefs.IsRateLimit(err), errdefs.IsBudgetExceeded(err):
		return CallError_RESOURCE_EXHAUSTED
	case errdefs.IsNotAvailable(err), errdefs.IsInterrupted(err):
		return CallError_UNAVAILABLE
	default:
		return CallError_INTERNAL
	}
}

// RetryableOf reports whether the class of err is one a retry may fix.
// It is the default a plugin's own CallError.retryable overrides, not a
// replacement for it: the plugin knows more about its own failures than
// their class does.
func RetryableOf(err error) bool {
	switch KindOf(err) {
	case CallError_TIMEOUT, CallError_RESOURCE_EXHAUSTED, CallError_UNAVAILABLE:
		return true
	default:
		return false
	}
}

// NewCallError renders err as the wire error a plugin sends back.
func NewCallError(err error, callID string) *CallError {
	if err == nil {
		return nil
	}
	event := &CallError{
		Kind:      KindOf(err),
		Message:   err.Error(),
		Retryable: RetryableOf(err),
		CallId:    callID,
	}
	// The class is not re-derivable from anything else, so an error
	// that carries a structured detail keeps it on the wire.
	if detailed, ok := err.(interface{ Details() any }); ok {
		if payload, marshalErr := json.Marshal(detailed.Details()); marshalErr == nil {
			event.DetailsJson = payload
		}
	}
	return event
}

// ErrorOf turns a wire error back into an error of the class the plugin
// declared, so middleware on the craft side (retry, timeout, redaction)
// keeps treating a remote failure like a local one.
func ErrorOf(event *CallError) error {
	if event == nil {
		return nil
	}
	var base error
	if message := event.GetMessage(); message != "" {
		base = errString(message)
	} else {
		base = errString("plugin call failed")
	}
	switch event.GetKind() {
	case CallError_INVALID_ARGUMENT:
		return errdefs.Validation(base)
	case CallError_FAILED_PRECONDITION:
		return errdefs.Aborted(base)
	case CallError_NOT_FOUND:
		return errdefs.NotFound(base)
	case CallError_TIMEOUT:
		return errdefs.Timeout(base)
	case CallError_RESOURCE_EXHAUSTED:
		return errdefs.RateLimit(base)
	case CallError_PERMISSION_DENIED:
		return errdefs.Forbidden(base)
	case CallError_UNAVAILABLE:
		return errdefs.NotAvailable(base)
	case CallError_INTERNAL, CallError_UNKNOWN:
		return errdefs.Internal(base)
	default:
		return errdefs.Internal(base)
	}
}

// errString is the message carrier ErrorOf wraps: it keeps the plugin's
// text without pretending to be a class of its own.
type errString string

func (e errString) Error() string { return string(e) }
