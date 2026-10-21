package protocol

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestKindOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want CallError_Kind
	}{
		{name: "nil", err: nil, want: CallError_UNKNOWN},
		{name: "validation", err: errdefs.Validationf("bad"), want: CallError_INVALID_ARGUMENT},
		{name: "not found", err: errdefs.NotFoundf("gone"), want: CallError_NOT_FOUND},
		{name: "unauthorized", err: errdefs.Unauthorizedf("who"), want: CallError_PERMISSION_DENIED},
		{name: "forbidden", err: errdefs.Forbiddenf("no"), want: CallError_PERMISSION_DENIED},
		{name: "policy", err: errdefs.PolicyDeniedf("no"), want: CallError_PERMISSION_DENIED},
		{name: "conflict", err: errdefs.Conflictf("dup"), want: CallError_FAILED_PRECONDITION},
		{name: "aborted", err: errdefs.Abortedf("stop"), want: CallError_FAILED_PRECONDITION},
		{name: "timeout", err: errdefs.Timeoutf("slow"), want: CallError_TIMEOUT},
		{name: "rate limit", err: errdefs.RateLimitf("many"), want: CallError_RESOURCE_EXHAUSTED},
		{name: "budget", err: errdefs.BudgetExceededf("spent"), want: CallError_RESOURCE_EXHAUSTED},
		{name: "unavailable", err: errdefs.NotAvailablef("down"), want: CallError_UNAVAILABLE},
		{name: "interrupted", err: errdefs.Interruptedf("stop"), want: CallError_UNAVAILABLE},
		{name: "plain", err: errors.New("boom"), want: CallError_INTERNAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.err); got != tc.want {
				t.Fatalf("KindOf(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryableOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "timeout", err: errdefs.Timeoutf("slow"), want: true},
		{name: "rate limit", err: errdefs.RateLimitf("many"), want: true},
		{name: "unavailable", err: errdefs.NotAvailablef("down"), want: true},
		{name: "validation", err: errdefs.Validationf("bad")},
		{name: "plain", err: errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryableOf(tc.err); got != tc.want {
				t.Fatalf("RetryableOf(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// detailedError carries a structured detail, the way an application error
// whose caller can act on it does.
type detailedError struct{ hint string }

func (e detailedError) Error() string { return "detailed: " + e.hint }
func (e detailedError) Details() any  { return map[string]string{"hint": e.hint} }

func TestNewCallError(t *testing.T) {
	if event := NewCallError(nil, "call-1"); event != nil {
		t.Fatalf("NewCallError(nil) = %v, want nil", event)
	}
	event := NewCallError(errdefs.Validationf("bad input"), "call-1")
	if got, want := event.GetKind(), CallError_INVALID_ARGUMENT; got != want {
		t.Fatalf("kind = %v, want %v", got, want)
	}
	if event.GetRetryable() {
		t.Fatal("a validation error is not retryable")
	}
	if got, want := event.GetCallId(), "call-1"; got != want {
		t.Fatalf("call id = %q, want %q", got, want)
	}
	if got, want := event.GetMessage(), "bad input"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if len(event.GetDetailsJson()) != 0 {
		t.Fatalf("details = %s, want none", event.GetDetailsJson())
	}

	detailed := NewCallError(detailedError{hint: "retry later"}, "call-2")
	if got, want := string(detailed.GetDetailsJson()), `{"hint":"retry later"}`; got != want {
		t.Fatalf("details = %s, want %s", got, want)
	}

	timeout := NewCallError(errdefs.Timeoutf("slow"), "call-3")
	if !timeout.GetRetryable() {
		t.Fatal("a timeout is retryable")
	}
}

func TestErrorOf(t *testing.T) {
	if err := ErrorOf(nil); err != nil {
		t.Fatalf("ErrorOf(nil) = %v, want nil", err)
	}
	cases := []struct {
		name  string
		event *CallError
		check func(error) bool
	}{
		{
			name:  "invalid argument",
			event: &CallError{Kind: CallError_INVALID_ARGUMENT, Message: "bad"},
			check: errdefs.IsValidation,
		},
		{
			name:  "failed precondition",
			event: &CallError{Kind: CallError_FAILED_PRECONDITION, Message: "conflict"},
			check: errdefs.IsAborted,
		},
		{
			name:  "not found",
			event: &CallError{Kind: CallError_NOT_FOUND, Message: "gone"},
			check: errdefs.IsNotFound,
		},
		{
			name:  "timeout",
			event: &CallError{Kind: CallError_TIMEOUT, Message: "slow"},
			check: errdefs.IsTimeout,
		},
		{
			name:  "resource exhausted",
			event: &CallError{Kind: CallError_RESOURCE_EXHAUSTED, Message: "many"},
			check: errdefs.IsRateLimit,
		},
		{
			name:  "permission denied",
			event: &CallError{Kind: CallError_PERMISSION_DENIED, Message: "no"},
			check: errdefs.IsForbidden,
		},
		{
			name:  "unavailable",
			event: &CallError{Kind: CallError_UNAVAILABLE, Message: "down"},
			check: errdefs.IsNotAvailable,
		},
		{
			name:  "internal",
			event: &CallError{Kind: CallError_INTERNAL, Message: "boom"},
			check: errdefs.IsInternal,
		},
		{
			name:  "unknown",
			event: &CallError{Kind: CallError_UNKNOWN, Message: "?"},
			check: errdefs.IsInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ErrorOf(tc.event)
			if !tc.check(err) {
				t.Fatalf("ErrorOf(%v) = %v, wrong class", tc.event, err)
			}
			if got, want := err.Error(), tc.event.GetMessage(); got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
		})
	}
}

func TestErrorOfNamesTheFailureWithoutAMessage(t *testing.T) {
	err := ErrorOf(&CallError{Kind: CallError_UNAVAILABLE})
	if got, want := err.Error(), "plugin call failed"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestCallErrorRoundTripKeepsTheClass(t *testing.T) {
	// The classes craft reasons about have to survive both directions of
	// the wire; a remote failure that lands in a different class would be
	// retried, or not, for the wrong reason.
	cases := []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{name: "validation", err: errdefs.Validationf("bad"), check: errdefs.IsValidation},
		{name: "not found", err: errdefs.NotFoundf("gone"), check: errdefs.IsNotFound},
		{name: "aborted", err: errdefs.Abortedf("stop"), check: errdefs.IsAborted},
		{name: "timeout", err: errdefs.Timeoutf("slow"), check: errdefs.IsTimeout},
		{name: "rate limit", err: errdefs.RateLimitf("many"), check: errdefs.IsRateLimit},
		{name: "forbidden", err: errdefs.Forbiddenf("no"), check: errdefs.IsForbidden},
		{name: "unavailable", err: errdefs.NotAvailablef("down"), check: errdefs.IsNotAvailable},
		{name: "internal", err: errdefs.Internalf("boom"), check: errdefs.IsInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			round := ErrorOf(NewCallError(tc.err, "call-1"))
			if !tc.check(round) {
				t.Fatalf("%v came back as %v, wrong class", tc.err, round)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	// The endpoint form is what the resolver accepts on every platform;
	// the "unix:///" form reads a Windows drive letter as an authority.
	if got, want := Target("/tmp/wire.sock"), "unix:/tmp/wire.sock"; got != want {
		t.Fatalf("Target = %q, want %q", got, want)
	}
}

func TestEndpointReadsTheAdvertisedSocket(t *testing.T) {
	t.Setenv(WireEnv, "")
	if _, err := Endpoint(); !errdefs.IsValidation(err) {
		t.Fatalf("Endpoint with no advertisement = %v, want a validation error", err)
	}
	t.Setenv(WireEnv, "/tmp/wire.sock")
	endpoint, err := Endpoint()
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if endpoint != "/tmp/wire.sock" {
		t.Fatalf("endpoint = %q, want the advertised socket", endpoint)
	}
}
