package errdefs

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// kindTable is the single source for the spelling, the constructor and the HTTP
// status of every category, so Kind and HTTPStatus cannot drift apart: the
// status column is asserted against HTTPStatus itself rather than restated.
var kindTable = []struct {
	kind    Kind
	name    string
	wrap    func(error) error
	status  int
	tripped bool // whether KindOf must report this kind for wrap(errors.New("x"))
}{
	{KindNone, "none", nil, http.StatusInternalServerError, true},
	{KindNotFound, "not_found", NotFound, http.StatusNotFound, true},
	{KindValidation, "validation", Validation, http.StatusBadRequest, true},
	{KindUnauthorized, "unauthorized", Unauthorized, http.StatusUnauthorized, true},
	{KindForbidden, "forbidden", Forbidden, http.StatusForbidden, true},
	{KindConflict, "conflict", Conflict, http.StatusConflict, true},
	{KindRateLimit, "rate_limit", RateLimit, http.StatusTooManyRequests, true},
	{KindTimeout, "timeout", Timeout, http.StatusGatewayTimeout, true},
	{KindInterrupted, "interrupted", Interrupted, http.StatusConflict, true},
	{KindAborted, "aborted", Aborted, http.StatusConflict, true},
	{KindNotAvailable, "not_available", NotAvailable, http.StatusServiceUnavailable, true},
	{KindInternal, "internal", Internal, http.StatusInternalServerError, true},
	{KindBudgetExceeded, "budget_exceeded", BudgetExceeded, http.StatusTooManyRequests, true},
	{KindPolicyDenied, "policy_denied", PolicyDenied, http.StatusForbidden, true},
}

func TestKind_ClassifiesEveryCategory(t *testing.T) {
	for _, tc := range kindTable {
		if tc.wrap == nil {
			continue
		}
		err := tc.wrap(errors.New("boom"))
		if got := KindOf(err); got != tc.kind {
			t.Errorf("%s: KindOf = %v, want %v", tc.name, got, tc.kind)
		}
	}
}

func TestKind_StringRoundTrip(t *testing.T) {
	for _, tc := range kindTable {
		if got := tc.kind.String(); got != tc.name {
			t.Errorf("Kind %d: String = %q, want %q", tc.kind, got, tc.name)
		}
		back, ok := ParseKind(tc.name)
		if !ok {
			t.Errorf("%q: ParseKind reported an unknown name", tc.name)
			continue
		}
		if back != tc.kind {
			t.Errorf("%q: ParseKind = %v, want %v", tc.name, back, tc.kind)
		}
	}
}

// An unknown spelling must be reported, not folded into KindNone: a receiver
// that cannot classify an error has to say so.
func TestKind_ParseRejectsUnknown(t *testing.T) {
	if _, ok := ParseKind("not_a_kind"); ok {
		t.Fatal("ParseKind accepted an unknown name")
	}
	if k, ok := ParseKind(""); !ok || k != KindNone {
		t.Fatalf("ParseKind(\"\") = %v, %v; want KindNone, true", k, ok)
	}
	if got := Kind(200).String(); got != "kind(200)" {
		t.Fatalf("out-of-range String = %q, want kind(200)", got)
	}
}

// Kind.Error is the inverse of KindOf — that is its whole job, since the wire
// decodes a kind and has to rebuild an error carrying it.
func TestKind_ErrorRebuildsTheClassification(t *testing.T) {
	for _, tc := range kindTable {
		err := tc.kind.Error("remote refused")
		if got := KindOf(err); got != tc.kind {
			t.Errorf("%s: KindOf(Error) = %v, want %v", tc.name, got, tc.kind)
		}
		if err.Error() != "remote refused" {
			t.Errorf("%s: text = %q", tc.name, err.Error())
		}
	}
}

// The wire carries a kind and a status is derived from it; the two must never
// disagree about which category a chain "is".
func TestKind_AgreesWithHTTPStatus(t *testing.T) {
	for _, tc := range kindTable {
		if tc.wrap == nil {
			if got := HTTPStatus(nil); got != http.StatusOK {
				t.Errorf("HTTPStatus(nil) = %d", got)
			}
			continue
		}
		err := tc.wrap(errors.New("boom"))
		if got := HTTPStatus(err); got != tc.status {
			t.Errorf("%s: HTTPStatus = %d, want %d", tc.name, got, tc.status)
		}
	}
}

// A multi-classified chain resolves to one category, in the documented order.
// The case worth pinning is the one a policy layer produces: a policy refusal
// that happened to time out is still a policy refusal.
func TestKind_PriorityOnMultiClassifiedChain(t *testing.T) {
	inner := Timeout(errors.New("deadline"))
	err := PolicyDenied(inner)

	if got := KindOf(err); got != KindPolicyDenied {
		t.Fatalf("KindOf = %v, want %v", got, KindPolicyDenied)
	}
	if got := HTTPStatus(err); got != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want 403 — Kind and HTTPStatus disagree", got)
	}
	// The inner classification stays reachable through the chain.
	if !IsTimeout(err) {
		t.Fatal("the inner Timeout classification was lost")
	}
}

func TestKind_SurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("transport: %w", NotFound(errors.New("gone")))
	if got := KindOf(err); got != KindNotFound {
		t.Fatalf("KindOf = %v, want %v", got, KindNotFound)
	}
}

func TestKind_PlainErrorsAreNone(t *testing.T) {
	for _, err := range []error{nil, errors.New("plain"), fmt.Errorf("wrapped: %w", errors.New("plain"))} {
		if got := KindOf(err); got != KindNone {
			t.Errorf("KindOf(%v) = %v, want KindNone", err, got)
		}
	}
}
