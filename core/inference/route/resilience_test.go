package route

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestNonRetryableKindUndefinedTool(t *testing.T) {
	if !nonRetryableKind(inference.UndefinedTool) {
		t.Fatal("undefined tool rejections must be non-retryable")
	}
	if !nonRetryableKind(inference.InvalidProviderResponse) {
		t.Fatal("invalid provider responses must stay non-retryable")
	}
	if nonRetryableKind(inference.ProviderFailure) {
		t.Fatal("provider failures must remain retryable candidates")
	}
}

// TestProviderTruncatedNeverDefaultRetryable locks in the D1 contract:
// core never auto-retries a truncated stream, so the default predicate
// rejects ProviderTruncated even though the error classifies as not
// available and no output was observed.
func TestProviderTruncatedNeverDefaultRetryable(t *testing.T) {
	infErr := inference.NewError(
		inference.ProviderTruncated, inference.OperationGenerate, "",
		errors.New("stream ended without a finish reason"))
	decision := RetryDecision{
		Operation: inference.OperationGenerate,
		Phase:     AttemptPhaseExecute,
		ErrorKind: inference.ProviderTruncated,
		Err:       infErr,
		Attempt:   0,
	}
	if DefaultRetryable(context.Background(), decision) {
		t.Fatal("provider_truncated must never be retried by DefaultRetryable")
	}
	policy, err := (&RetryPolicyConfig{
		MaxAttempts: 2,
		Retryable:   []RetryableClass{RetryableUnavailable},
	}).policy()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if retryEligible(context.Background(), policy, decision) {
		t.Fatal("provider_truncated must not be retried even when retryable_unavailable is configured")
	}
}
