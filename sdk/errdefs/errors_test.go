package errdefs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestMark_NilPassthrough(t *testing.T) {
	if NotFound(nil) != nil {
		t.Fatal("NotFound(nil) should return nil")
	}
	if Timeout(nil) != nil {
		t.Fatal("Timeout(nil) should return nil")
	}
	if Internal(nil) != nil {
		t.Fatal("Internal(nil) should return nil")
	}
}

func TestCategory_CheckFunctions(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"NotFound", NotFoundf("user %q", "x"), IsNotFound},
		{"Validation", Validationf("bad input"), IsValidation},
		{"Unauthorized", Unauthorizedf("no token"), IsUnauthorized},
		{"Forbidden", Forbiddenf("no access"), IsForbidden},
		{"Conflict", Conflictf("duplicate"), IsConflict},
		{"RateLimit", RateLimitf("slow down"), IsRateLimit},
		{"Timeout", Timeoutf("deadline"), IsTimeout},
		{"Interrupted", Interruptedf("paused"), IsInterrupted},
		{"Aborted", Abortedf("cancelled"), IsAborted},
		{"NotAvailable", NotAvailablef("down"), IsNotAvailable},
		{"Internal", Internalf("boom"), IsInternal},
		{"BudgetExceeded", BudgetExceededf("over %d tokens", 1000), IsBudgetExceeded},
		{"PolicyDenied", PolicyDeniedf("tool %q not allowed", "shell"), IsPolicyDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.check(tt.err) {
				t.Fatalf("Is%s should be true for %v", tt.name, tt.err)
			}
		})
	}
}

func TestCategory_WrapExisting(t *testing.T) {
	cause := fmt.Errorf("connection refused")
	err := NotFound(cause)
	if !IsNotFound(err) {
		t.Fatal("should be NotFound")
	}
	if Unwrap(err) != cause {
		t.Fatal("Unwrap should return cause")
	}
	if err.Error() != "connection refused" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "connection refused")
	}
}

func TestCategory_NegativeCheck(t *testing.T) {
	err := NotFoundf("missing")
	if IsValidation(err) {
		t.Fatal("NotFound should not be Validation")
	}
	if IsTimeout(err) {
		t.Fatal("NotFound should not be Timeout")
	}
}

func TestCategory_PlainErrorIsNone(t *testing.T) {
	err := fmt.Errorf("plain error")
	checks := []struct {
		name string
		fn   func(error) bool
	}{
		{"NotFound", IsNotFound},
		{"Validation", IsValidation},
		{"Timeout", IsTimeout},
		{"Interrupted", IsInterrupted},
		{"Internal", IsInternal},
	}
	for _, c := range checks {
		if c.fn(err) {
			t.Fatalf("plain error should not be %s", c.name)
		}
	}
}

func TestCategory_SurvivesWrapping(t *testing.T) {
	inner := Validationf("field required")
	outer := fmt.Errorf("create user: %w", inner)
	if !IsValidation(outer) {
		t.Fatal("category should survive fmt.Errorf wrapping")
	}
}

func TestCategory_DoubleWrap(t *testing.T) {
	err := NotFound(Validation(fmt.Errorf("weird")))
	if !IsNotFound(err) {
		t.Fatal("outer NotFound should match")
	}
	if !IsValidation(err) {
		t.Fatal("inner Validation should also match via Unwrap chain")
	}
}

func TestSentinel_WorksWithCategory(t *testing.T) {
	ErrNotExist := Interrupted(New("execution interrupted"))
	wrapped := fmt.Errorf("node x: %w", ErrNotExist)
	if !Is(wrapped, ErrNotExist) {
		t.Fatal("errors.Is should find sentinel through wrapping")
	}
	if !IsInterrupted(wrapped) {
		t.Fatal("category should also be detectable")
	}
}

func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{
		{nil, http.StatusOK},
		{NotFoundf("x"), http.StatusNotFound},
		{Validationf("x"), http.StatusBadRequest},
		{Unauthorizedf("x"), http.StatusUnauthorized},
		{Forbiddenf("x"), http.StatusForbidden},
		{Conflictf("x"), http.StatusConflict},
		{RateLimitf("x"), http.StatusTooManyRequests},
		{Timeoutf("x"), http.StatusGatewayTimeout},
		{NotAvailablef("x"), http.StatusServiceUnavailable},
		{Interruptedf("x"), http.StatusConflict},
		{Abortedf("x"), http.StatusConflict},
		{Internalf("x"), http.StatusInternalServerError},
		{BudgetExceededf("x"), http.StatusTooManyRequests},
		{PolicyDeniedf("x"), http.StatusForbidden},
		{fmt.Errorf("plain"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		name := "nil"
		if tt.err != nil {
			name = tt.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			got := HTTPStatus(tt.err)
			if got != tt.status {
				t.Fatalf("HTTPStatus = %d, want %d", got, tt.status)
			}
		})
	}
}

func TestHTTPStatus_SurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", NotFoundf("inner"))
	if HTTPStatus(err) != http.StatusNotFound {
		t.Fatalf("HTTPStatus through wrapping = %d", HTTPStatus(err))
	}
}

func TestBudgetExceeded_NotConfusedWithRateLimit(t *testing.T) {
	// Both map to HTTP 429 but represent different intents; the
	// classification predicates MUST stay disjoint so handlers that
	// branch on intent still get unambiguous answers.
	be := BudgetExceededf("token cap")
	if IsRateLimit(be) {
		t.Fatal("BudgetExceeded should not be RateLimit")
	}
	if !IsBudgetExceeded(be) {
		t.Fatal("BudgetExceeded should be itself")
	}

	rl := RateLimitf("upstream 429")
	if IsBudgetExceeded(rl) {
		t.Fatal("RateLimit should not be BudgetExceeded")
	}
}

func TestPolicyDenied_NotConfusedWithForbidden(t *testing.T) {
	// Both map to HTTP 403 but represent different intents; same
	// disjoint-classification rationale as above.
	pd := PolicyDeniedf("tool %q blocked", "shell")
	if IsForbidden(pd) {
		t.Fatal("PolicyDenied should not be Forbidden")
	}
	if !IsPolicyDenied(pd) {
		t.Fatal("PolicyDenied should be itself")
	}

	fb := Forbiddenf("upstream 403")
	if IsPolicyDenied(fb) {
		t.Fatal("Forbidden should not be PolicyDenied")
	}
}

func TestBudgetExceeded_SurvivesWrapping(t *testing.T) {
	inner := BudgetExceededf("over budget")
	outer := fmt.Errorf("llm call: %w", inner)
	if !IsBudgetExceeded(outer) {
		t.Fatal("BudgetExceeded should survive fmt.Errorf wrapping")
	}
}

func TestPolicyDenied_SurvivesWrapping(t *testing.T) {
	inner := PolicyDeniedf("network egress not allowed")
	outer := fmt.Errorf("dispatch tool: %w", inner)
	if !IsPolicyDenied(outer) {
		t.Fatal("PolicyDenied should survive fmt.Errorf wrapping")
	}
}

func TestStdlibReexports(t *testing.T) {
	sentinel := New("sentinel")
	wrapped := fmt.Errorf("wrap: %w", sentinel)
	if !Is(wrapped, sentinel) {
		t.Fatal("Is should delegate to stdlib")
	}
	inner := Unwrap(wrapped)
	if inner != sentinel {
		t.Fatal("Unwrap should delegate to stdlib")
	}
}

// TestFromContext_Mapping pins the context.Err() → errdefs translation.
// Pod-level SLO accounting depends on these specific buckets:
//   - DeadlineExceeded → Timeout (counts against latency SLO)
//   - Canceled         → Aborted (does not count; user-initiated stop)
//
// Drifting either mapping would silently corrupt operator dashboards;
// bumping this test forces an explicit acknowledgement.
func TestFromContext_Mapping(t *testing.T) {
	if got := FromContext(context.DeadlineExceeded); !IsTimeout(got) {
		t.Errorf("DeadlineExceeded → got %v, want IsTimeout", got)
	}
	if got := FromContext(context.Canceled); !IsAborted(got) {
		t.Errorf("Canceled → got %v, want IsAborted", got)
	}
}

// TestFromContext_PreservesIdentity guarantees that callers can still
// match against the underlying context sentinel after classification.
// Without this, retry/back-off code that runs `errors.Is(err,
// context.DeadlineExceeded)` would silently break.
func TestFromContext_PreservesIdentity(t *testing.T) {
	got := FromContext(context.DeadlineExceeded)
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("FromContext lost the underlying sentinel: %v", got)
	}
}

// TestFromContext_NilPassthrough lets callers run FromContext(ctx.Err())
// unconditionally on every path without inventing a nil branch.
func TestFromContext_NilPassthrough(t *testing.T) {
	if got := FromContext(nil); got != nil {
		t.Errorf("FromContext(nil) = %v, want nil", got)
	}
}

// TestFromContext_DoesNotOverrideExistingClass proves that an error
// already carrying an errdefs marker survives FromContext intact. This
// is what lets stream wrappers do `s.err = errdefs.FromContext(s.err)`
// safely even after the error has been classified upstream.
func TestFromContext_DoesNotOverrideExistingClass(t *testing.T) {
	pre := Internalf("downstream blew up")
	got := FromContext(pre)
	if !IsInternal(got) || IsTimeout(got) {
		t.Errorf("FromContext clobbered classification: %v", got)
	}
}

// TestFromContext_PassesThroughUnknown handles the case where a caller
// pulls context.Cause(ctx) and gets a custom cancellation reason. We
// must not pretend it is a Timeout / Aborted; surface the original.
func TestFromContext_PassesThroughUnknown(t *testing.T) {
	custom := errors.New("custom cause")
	got := FromContext(custom)
	if got != custom {
		t.Errorf("FromContext mutated unknown error: got %v, want %v", got, custom)
	}
	if IsTimeout(got) || IsAborted(got) {
		t.Errorf("FromContext over-classified unknown error: %v", got)
	}
}
