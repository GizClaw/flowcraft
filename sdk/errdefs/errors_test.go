package errdefs

import (
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
