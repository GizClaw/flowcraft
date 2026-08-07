package route

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestErrorClassificationCoversEveryKind(t *testing.T) {
	kinds := []struct {
		kind  ErrorKind
		check func(error) bool
	}{
		{InvalidRequest, errdefs.IsValidation},
		{SelectorUnavailable, errdefs.IsNotAvailable},
		{NoRoute, errdefs.IsNotAvailable},
		{SelectionFailed, errdefs.IsNotAvailable},
		{SelectorContractViolation, errdefs.IsInternal},
		{FallbackFailed, errdefs.IsNotAvailable},
		{FallbackContractViolation, errdefs.IsInternal},
		{FallbackLimitExceeded, errdefs.IsInternal},
		{CircuitOpen, errdefs.IsNotAvailable},
	}
	for _, item := range kinds {
		err := NewError(item.kind, inference.OperationGenerate, errors.New("cause"))
		if !item.check(err) {
			t.Errorf("%s is not classified as expected: %v", item.kind, err)
		}
		if !IsKind(err, item.kind) {
			t.Errorf("IsKind(%v, %s) = false", err, item.kind)
		}
		if err.Operation != inference.OperationGenerate {
			t.Errorf("%s operation = %q", item.kind, err.Operation)
		}
	}
}

func TestSelectionAndFallbackFailuresPreserveCauseClassification(t *testing.T) {
	causes := []struct {
		name  string
		cause error
		check func(error) bool
	}{
		{"timeout", context.DeadlineExceeded, errdefs.IsTimeout},
		{"cancel", context.Canceled, errdefs.IsAborted},
		{"rate limit", errdefs.RateLimit(errors.New("slow down")), errdefs.IsRateLimit},
		{"plain", errors.New("unknown"), errdefs.IsNotAvailable},
	}
	for _, item := range causes {
		t.Run("selection/"+item.name, func(t *testing.T) {
			err := NewError(SelectionFailed, inference.OperationEmbed, item.cause)
			if !item.check(err) {
				t.Fatalf("SelectionFailed(%v) lost cause classification", item.cause)
			}
		})
		t.Run("fallback/"+item.name, func(t *testing.T) {
			err := NewError(FallbackFailed, inference.OperationGenerate, item.cause)
			if !item.check(err) {
				t.Fatalf("FallbackFailed(%v) lost cause classification", item.cause)
			}
		})
	}
}

func TestFallbackEligibilityIsSingleSourced(t *testing.T) {
	// The allowlist is local compiler rejections only; everything else is a
	// provider-visible failure that must never be retried on another target.
	eligible := []inference.ErrorKind{
		inference.UnsupportedOperation,
		inference.UnsupportedFeature,
		inference.InvalidExtension,
	}
	prohibited := []inference.ErrorKind{
		inference.InvalidRequest,
		inference.UnknownProvider,
		inference.UnknownModel,
		inference.UnknownProfile,
		inference.PolicyDenied,
		inference.OperationInterrupted,
		inference.CompilerContractViolation,
		inference.ProviderFailure,
		inference.InvalidProviderResponse,
		"",
	}
	for _, kind := range eligible {
		if !fallbackEligibleKind(kind) {
			t.Errorf("%s is not fallback eligible", kind)
		}
	}
	for _, kind := range prohibited {
		if fallbackEligibleKind(kind) {
			t.Errorf("%s is fallback eligible", kind)
		}
	}
}

func TestErrorMessageStaysStructural(t *testing.T) {
	cause := errors.New("sk-secret-key-value")
	err := NewError(SelectorContractViolation, inference.OperationRealtime, cause)
	if err.Error() == cause.Error() {
		t.Fatal("route error message exposes the cause")
	}
	if got := err.Error(); got != "selector_contract_violation during realtime" {
		t.Fatalf("error message = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap lost the cause")
	}
}
