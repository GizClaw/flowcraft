package errdefs

import (
	"errors"
	"strconv"
)

// Kind is a classification as a value: the answer KindOf gives, and the form a
// classification takes when it leaves the process.
//
// errdefs classifies by marker interface, which is the right shape for control
// flow ("does this error mean retry?") but not for a wire, where the receiver
// has no Go types to match against. Kind is that middle ground: KindOf reads the
// chain once, and Kind.Error rebuilds an error carrying the same classification
// on the other side.
//
// An error can carry several markers — PolicyDenied(Timeout(err)) is a policy
// refusal that happened to time out. KindOf answers with the first match in the
// fixed order below, which is exactly the order HTTPStatus uses, so a gateway
// mapping a status and a proxy carrying a kind never disagree about which
// category an error "is".
type Kind uint8

const (
	// KindNone is an error with no errdefs classification.
	KindNone Kind = iota
	KindNotFound
	KindValidation
	KindUnauthorized
	KindForbidden
	KindConflict
	KindRateLimit
	KindTimeout
	KindInterrupted
	KindAborted
	KindNotAvailable
	KindInternal
	KindBudgetExceeded
	KindPolicyDenied
)

// kinds is the single spelling table: String, ParseKind and tests all read it,
// so a new category cannot be added to one and forgotten in the others.
var kinds = map[Kind]string{
	KindNone:           "none",
	KindNotFound:       "not_found",
	KindValidation:     "validation",
	KindUnauthorized:   "unauthorized",
	KindForbidden:      "forbidden",
	KindConflict:       "conflict",
	KindRateLimit:      "rate_limit",
	KindTimeout:        "timeout",
	KindInterrupted:    "interrupted",
	KindAborted:        "aborted",
	KindNotAvailable:   "not_available",
	KindInternal:       "internal",
	KindBudgetExceeded: "budget_exceeded",
	KindPolicyDenied:   "policy_denied",
}

// String returns the wire spelling of k, or "kind(<n>)" for a value outside the
// defined set (a corrupted wire value, in practice).
func (k Kind) String() string {
	if name, ok := kinds[k]; ok {
		return name
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// ParseKind is the inverse of [Kind.String]. An unknown name is reported rather
// than silently mapped to KindNone: a receiver that cannot classify an error
// should say so, not pretend the error had no classification.
func ParseKind(name string) (Kind, bool) {
	if name == "" {
		return KindNone, true
	}
	for kind, spelling := range kinds {
		if spelling == name {
			return kind, true
		}
	}
	return KindNone, false
}

// KindOf reports the classification of err, or KindNone when it has none.
//
// The order below is [HTTPStatus]'s order, and it matters: it decides which
// category wins when an error carries more than one marker.
func KindOf(err error) Kind {
	switch {
	case err == nil:
		return KindNone
	case IsNotFound(err):
		return KindNotFound
	case IsValidation(err):
		return KindValidation
	case IsUnauthorized(err):
		return KindUnauthorized
	case IsForbidden(err):
		return KindForbidden
	case IsConflict(err):
		return KindConflict
	case IsRateLimit(err):
		return KindRateLimit
	case IsBudgetExceeded(err):
		return KindBudgetExceeded
	case IsPolicyDenied(err):
		return KindPolicyDenied
	case IsTimeout(err):
		return KindTimeout
	case IsNotAvailable(err):
		return KindNotAvailable
	case IsInternal(err):
		return KindInternal
	case IsInterrupted(err):
		return KindInterrupted
	case IsAborted(err):
		return KindAborted
	default:
		return KindNone
	}
}

// Error builds an error of this kind carrying msg. KindNone returns a plain
// error, which is what KindOf will report as KindNone again.
func (k Kind) Error(msg string) error {
	err := errors.New(msg)
	switch k {
	case KindNotFound:
		return NotFound(err)
	case KindValidation:
		return Validation(err)
	case KindUnauthorized:
		return Unauthorized(err)
	case KindForbidden:
		return Forbidden(err)
	case KindConflict:
		return Conflict(err)
	case KindRateLimit:
		return RateLimit(err)
	case KindTimeout:
		return Timeout(err)
	case KindInterrupted:
		return Interrupted(err)
	case KindAborted:
		return Aborted(err)
	case KindNotAvailable:
		return NotAvailable(err)
	case KindInternal:
		return Internal(err)
	case KindBudgetExceeded:
		return BudgetExceeded(err)
	case KindPolicyDenied:
		return PolicyDenied(err)
	default:
		return err
	}
}
