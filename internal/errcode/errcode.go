// Package errcode provides code-based errors for the FlowCraft platform HTTP layer.
// SDK-facing classification remains in sdk/errdefs; platform-specific status codes
// and wire-level error codes live here without extending the SDK.
package errcode

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Wire-level error codes (JSON "error.code", stream sink, webhooks).
// Keep values stable: clients and logs may depend on these strings.
const (
	CodeNotFound         = "not_found"
	CodeValidationError  = "validation_error"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeConflict         = "conflict"
	CodeRateLimit        = "rate_limit"
	CodeTimeout          = "timeout"
	CodeInterrupted      = "interrupted"
	CodeAborted          = "aborted"
	CodeNotAvailable     = "not_available"
	CodeInternalError    = "internal_error"
	codeInternalAlias    = "internal" // accepted by FromCode / statusForCategory only
	CodePluginError      = "plugin_error"
	CodeMethodNotAllowed = "method_not_allowed"
)

// Error is a platform error with machine-readable code and HTTP status.
type Error struct {
	code    string
	status  int
	message string
	cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %s", e.message, e.cause.Error())
	}
	return e.message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Code returns the wire-level error code (e.g. [CodeNotFound]).
func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

// Status returns the HTTP status associated with this error.
func (e *Error) Status() int {
	if e == nil {
		return 0
	}
	return e.status
}

// Message returns the human-readable message without the wrapped cause text.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// New creates an error with the given code and HTTP status.
func New(code string, status int, message string) *Error {
	return &Error{code: code, status: status, message: message}
}

// Newf is like New with a formatted message.
func Newf(code string, status int, format string, args ...any) *Error {
	return &Error{code: code, status: status, message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code, HTTP status, and message to an existing error.
func Wrap(code string, status int, cause error, message string) *Error {
	if cause == nil {
		return nil
	}
	return &Error{code: code, status: status, message: message, cause: cause}
}

// Wrapf is like Wrap with a formatted message.
func Wrapf(code string, status int, cause error, format string, args ...any) *Error {
	if cause == nil {
		return nil
	}
	return &Error{code: code, status: status, message: fmt.Sprintf(format, args...), cause: cause}
}

// PluginErrorf wraps a plugin initialization failure as HTTP 422.
func PluginErrorf(format string, args ...any) *Error {
	return Newf(CodePluginError, http.StatusUnprocessableEntity, format, args...)
}

// PluginErrorWrap wraps cause with [CodePluginError] / 422.
func PluginErrorWrap(cause error, format string, args ...any) *Error {
	return Wrapf(CodePluginError, http.StatusUnprocessableEntity, cause, format, args...)
}

// MethodNotAllowedf is HTTP 405 with [CodeMethodNotAllowed].
func MethodNotAllowedf(format string, args ...any) *Error {
	return Newf(CodeMethodNotAllowed, http.StatusMethodNotAllowed, format, args...)
}

// FromCode maps a short category string (from stream.EventSink, etc.) to status + *Error.
func FromCode(code, message string) *Error {
	status := statusForCategory(code)
	return &Error{code: code, status: status, message: message}
}

func statusForCategory(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeValidationError:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeConflict:
		return http.StatusConflict
	case CodeRateLimit:
		return http.StatusTooManyRequests
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeInterrupted, CodeAborted:
		return http.StatusConflict
	case CodeNotAvailable:
		return http.StatusServiceUnavailable
	case CodeInternalError, codeInternalAlias:
		return http.StatusInternalServerError
	case CodePluginError:
		return http.StatusUnprocessableEntity
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	default:
		// Legacy: stream_loop used http.StatusText(500) as code for unknown errors.
		if code == http.StatusText(http.StatusInternalServerError) {
			return http.StatusInternalServerError
		}
		return http.StatusInternalServerError
	}
}

// Resolve returns wire code and HTTP status for any error.
// Platform *Error wins; otherwise sdk/errdefs markers are mapped.
func Resolve(err error) (code string, status int) {
	if err == nil {
		return "", http.StatusOK
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code(), pe.Status()
	}
	return errdefsCode(err), errdefs.HTTPStatus(err)
}

// PublicMessage returns text suitable for external JSON (message field).
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Message()
	}
	return err.Error()
}

func errdefsCode(err error) string {
	switch {
	case errdefs.IsNotFound(err):
		return CodeNotFound
	case errdefs.IsValidation(err):
		return CodeValidationError
	case errdefs.IsUnauthorized(err):
		return CodeUnauthorized
	case errdefs.IsForbidden(err):
		return CodeForbidden
	case errdefs.IsConflict(err):
		return CodeConflict
	case errdefs.IsRateLimit(err):
		return CodeRateLimit
	case errdefs.IsTimeout(err):
		return CodeTimeout
	case errdefs.IsInterrupted(err):
		return CodeInterrupted
	case errdefs.IsAborted(err):
		return CodeAborted
	case errdefs.IsNotAvailable(err):
		return CodeNotAvailable
	case errdefs.IsInternal(err):
		return CodeInternalError
	default:
		return CodeInternalError
	}
}
