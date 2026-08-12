// Package errdefs provides behavior-based error classification.
//
// Errors are classified by marker interfaces rather than error codes.
// Each category has three associated items:
//   - An unexported marker interface (e.g. interface{ NotFound() })
//   - A constructor that wraps/creates errors with the marker
//   - A check function (e.g. IsNotFound)
//
// Sentinel errors remain plain stdlib errors; classification is orthogonal
// to identity — errors.Is checks identity, IsXxx checks category.
package errdefs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Re-export standard library functions so callers only need one import.
var (
	Is     = errors.Is
	As     = errors.As
	Unwrap = errors.Unwrap
	Join   = errors.Join
)

// New creates a plain error (re-export of stdlib errors.New).
func New(text string) error { return errors.New(text) }

// Fmt creates a formatted error with optional wrapping (re-export of fmt.Errorf).
func Fmt(format string, args ...any) error { return fmt.Errorf(format, args...) }

// ---------------------------------------------------------------------------
// Marker interfaces — each defines a single behavior method.
// They are unexported so external packages cannot implement them directly;
// instead they use the constructors below.
// ---------------------------------------------------------------------------

type (
	notFound       interface{ NotFound() }
	validation     interface{ Validation() }
	unauthorized   interface{ Unauthorized() }
	forbidden      interface{ Forbidden() }
	conflict       interface{ Conflict() }
	rateLimit      interface{ RateLimit() }
	timeoutErr     interface{ IsTimeout() }
	interrupted    interface{ Interrupted() }
	aborted        interface{ Aborted() }
	notAvailable   interface{ NotAvailable() }
	internalErr    interface{ Internal() }
	budgetExceeded interface{ BudgetExceeded() }
	policyDenied   interface{ PolicyDenied() }
)

// ---------------------------------------------------------------------------
// Check functions
// ---------------------------------------------------------------------------

func IsNotFound(err error) bool     { var e notFound; return As(err, &e) }
func IsValidation(err error) bool   { var e validation; return As(err, &e) }
func IsUnauthorized(err error) bool { var e unauthorized; return As(err, &e) }
func IsForbidden(err error) bool    { var e forbidden; return As(err, &e) }
func IsConflict(err error) bool     { var e conflict; return As(err, &e) }
func IsRateLimit(err error) bool    { var e rateLimit; return As(err, &e) }
func IsTimeout(err error) bool      { var e timeoutErr; return As(err, &e) }
func IsInterrupted(err error) bool  { var e interrupted; return As(err, &e) }
func IsAborted(err error) bool      { var e aborted; return As(err, &e) }
func IsNotAvailable(err error) bool { var e notAvailable; return As(err, &e) }
func IsInternal(err error) bool     { var e internalErr; return As(err, &e) }

// IsBudgetExceeded reports whether err carries the BudgetExceeded
// classification — i.e. a per-pod / per-tenant token, cost, or quota
// limit has been hit. Use this to distinguish "we are stopping you on
// purpose" from external rate limits (IsRateLimit) and from generic
// internal failures (IsInternal). Maps to HTTP 429 by default.
func IsBudgetExceeded(err error) bool { var e budgetExceeded; return As(err, &e) }

// IsPolicyDenied reports whether err was produced by a policy-layer
// refusal — tool allow-list, network egress filter, role-based access
// check, etc. Distinguishes "you are not allowed to do this in this
// pod" from authentication failures (IsUnauthorized) and from external
// authorisation failures (IsForbidden). Maps to HTTP 403 by default.
func IsPolicyDenied(err error) bool { var e policyDenied; return As(err, &e) }

// ---------------------------------------------------------------------------
// Wrapper types — each embeds an error and adds a marker method.
// ---------------------------------------------------------------------------

type (
	errNotFound       struct{ error }
	errValidation     struct{ error }
	errUnauthorized   struct{ error }
	errForbidden      struct{ error }
	errConflict       struct{ error }
	errRateLimit      struct{ error }
	errTimeout        struct{ error }
	errInterrupted    struct{ error }
	errAborted        struct{ error }
	errNotAvailable   struct{ error }
	errInternal       struct{ error }
	errBudgetExceeded struct{ error }
	errPolicyDenied   struct{ error }
)

func (e errNotFound) Unwrap() error       { return e.error }
func (e errValidation) Unwrap() error     { return e.error }
func (e errUnauthorized) Unwrap() error   { return e.error }
func (e errForbidden) Unwrap() error      { return e.error }
func (e errConflict) Unwrap() error       { return e.error }
func (e errRateLimit) Unwrap() error      { return e.error }
func (e errTimeout) Unwrap() error        { return e.error }
func (e errInterrupted) Unwrap() error    { return e.error }
func (e errAborted) Unwrap() error        { return e.error }
func (e errNotAvailable) Unwrap() error   { return e.error }
func (e errInternal) Unwrap() error       { return e.error }
func (e errBudgetExceeded) Unwrap() error { return e.error }
func (e errPolicyDenied) Unwrap() error   { return e.error }

func (errNotFound) NotFound()             {}
func (errValidation) Validation()         {}
func (errUnauthorized) Unauthorized()     {}
func (errForbidden) Forbidden()           {}
func (errConflict) Conflict()             {}
func (errRateLimit) RateLimit()           {}
func (errTimeout) IsTimeout()             {}
func (errInterrupted) Interrupted()       {}
func (errAborted) Aborted()               {}
func (errNotAvailable) NotAvailable()     {}
func (errInternal) Internal()             {}
func (errBudgetExceeded) BudgetExceeded() {}
func (errPolicyDenied) PolicyDenied()     {}

// ---------------------------------------------------------------------------
// Constructors — wrap an existing error with a category marker.
//
// Each category has two forms:
//   Xxx(err)                  — wrap an existing error
//   Xxxf(format, args...)     — create a new error with formatting
// ---------------------------------------------------------------------------

func markOrNil[T error](wrap func(error) T, err error) error {
	if err == nil {
		return nil
	}
	return wrap(err)
}

func NotFound(err error) error {
	return markOrNil(func(e error) errNotFound { return errNotFound{e} }, err)
}

func Validation(err error) error {
	return markOrNil(func(e error) errValidation { return errValidation{e} }, err)
}

func Unauthorized(err error) error {
	return markOrNil(func(e error) errUnauthorized { return errUnauthorized{e} }, err)
}

func Forbidden(err error) error {
	return markOrNil(func(e error) errForbidden { return errForbidden{e} }, err)
}

func Conflict(err error) error {
	return markOrNil(func(e error) errConflict { return errConflict{e} }, err)
}

func RateLimit(err error) error {
	return markOrNil(func(e error) errRateLimit { return errRateLimit{e} }, err)
}

func Timeout(err error) error {
	return markOrNil(func(e error) errTimeout { return errTimeout{e} }, err)
}

func Interrupted(err error) error {
	return markOrNil(func(e error) errInterrupted { return errInterrupted{e} }, err)
}

func Aborted(err error) error {
	return markOrNil(func(e error) errAborted { return errAborted{e} }, err)
}

func NotAvailable(err error) error {
	return markOrNil(func(e error) errNotAvailable { return errNotAvailable{e} }, err)
}

func Internal(err error) error {
	return markOrNil(func(e error) errInternal { return errInternal{e} }, err)
}

// BudgetExceeded marks err as a budget / quota refusal — typically
// raised by a host (e.g. a sandbox host wrapping engine.UsageReporter)
// when a per-pod, per-tenant, or per-call token / cost / count limit
// has been hit. Use this instead of RateLimit for *internal* policy
// limits; RateLimit is for upstream / network rate-limit responses.
func BudgetExceeded(err error) error {
	return markOrNil(func(e error) errBudgetExceeded { return errBudgetExceeded{e} }, err)
}

// PolicyDenied marks err as a policy-layer refusal — tool allow-list
// rejection, network egress filter, role-based access check, etc.
// Use this instead of Forbidden for *internal* sandbox policy
// decisions; Forbidden is for upstream / authorisation responses.
func PolicyDenied(err error) error {
	return markOrNil(func(e error) errPolicyDenied { return errPolicyDenied{e} }, err)
}

func NotFoundf(format string, args ...any) error   { return NotFound(fmt.Errorf(format, args...)) }
func Validationf(format string, args ...any) error { return Validation(fmt.Errorf(format, args...)) }
func Unauthorizedf(format string, args ...any) error {
	return Unauthorized(fmt.Errorf(format, args...))
}
func Forbiddenf(format string, args ...any) error   { return Forbidden(fmt.Errorf(format, args...)) }
func Conflictf(format string, args ...any) error    { return Conflict(fmt.Errorf(format, args...)) }
func RateLimitf(format string, args ...any) error   { return RateLimit(fmt.Errorf(format, args...)) }
func Timeoutf(format string, args ...any) error     { return Timeout(fmt.Errorf(format, args...)) }
func Interruptedf(format string, args ...any) error { return Interrupted(fmt.Errorf(format, args...)) }
func Abortedf(format string, args ...any) error     { return Aborted(fmt.Errorf(format, args...)) }
func NotAvailablef(format string, args ...any) error {
	return NotAvailable(fmt.Errorf(format, args...))
}
func Internalf(format string, args ...any) error { return Internal(fmt.Errorf(format, args...)) }

func BudgetExceededf(format string, args ...any) error {
	return BudgetExceeded(fmt.Errorf(format, args...))
}

func PolicyDeniedf(format string, args ...any) error {
	return PolicyDenied(fmt.Errorf(format, args...))
}

// ---------------------------------------------------------------------------
// HTTP status mapping
// ---------------------------------------------------------------------------

// HTTPStatus returns the HTTP status code for an error based on its category.
// Unknown/uncategorized errors map to 500 Internal Server Error.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case IsNotFound(err):
		return http.StatusNotFound
	case IsValidation(err):
		return http.StatusBadRequest
	case IsUnauthorized(err):
		return http.StatusUnauthorized
	case IsForbidden(err):
		return http.StatusForbidden
	case IsConflict(err):
		return http.StatusConflict
	case IsRateLimit(err):
		return http.StatusTooManyRequests
	case IsBudgetExceeded(err):
		// Internal budget refusals share the 429 wire-shape with
		// upstream rate limits — clients SHOULD react identically
		// (back off, retry later) but operators can still tell them
		// apart by inspecting the error chain via IsBudgetExceeded.
		return http.StatusTooManyRequests
	case IsPolicyDenied(err):
		// Policy denials share the 403 wire-shape with upstream
		// Forbidden responses; same rationale as BudgetExceeded above.
		return http.StatusForbidden
	case IsTimeout(err):
		return http.StatusGatewayTimeout
	case IsNotAvailable(err):
		return http.StatusServiceUnavailable
	case IsInternal(err):
		return http.StatusInternalServerError
	case IsInterrupted(err), IsAborted(err):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// ---------------------------------------------------------------------------
// Context error normalisation
// ---------------------------------------------------------------------------

// FromContext maps a context error to the matching errdefs classification.
// It is the standard way for callers to surface a context.Done() failure
// so that pod / observability layers can distinguish a real timeout from
// a cooperative cancel:
//
//   - context.DeadlineExceeded → Timeout (HTTP 504; pod marks SLO miss).
//   - context.Canceled         → Aborted (HTTP 409; pod treats as user
//     stop, not a failure).
//
// FromContext returns the input error unchanged when it is nil, when it
// already satisfies one of the errdefs marker interfaces (so callers can
// fold ctx.Err() into ClassifyProviderError pipelines without losing the
// original classification), or when it is some other error value
// (defensive — context.Cause() can surface arbitrary cancellation
// causes).
func FromContext(err error) error {
	if err == nil {
		return nil
	}
	if HasClassification(err) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout(err)
	case errors.Is(err, context.Canceled):
		return Aborted(err)
	default:
		return err
	}
}

// HasClassification reports whether err already carries any errdefs
// behavioural marker. Used by FromContext and by ClassifyProviderError /
// ClassifyHTTPStatus (in http.go) to avoid double-wrapping a
// pre-classified error and changing its observable category. Returns
// false for nil.
func HasClassification(err error) bool {
	switch {
	case IsNotFound(err),
		IsValidation(err),
		IsUnauthorized(err),
		IsForbidden(err),
		IsConflict(err),
		IsRateLimit(err),
		IsTimeout(err),
		IsInterrupted(err),
		IsAborted(err),
		IsNotAvailable(err),
		IsInternal(err),
		IsBudgetExceeded(err),
		IsPolicyDenied(err):
		return true
	}
	return false
}
