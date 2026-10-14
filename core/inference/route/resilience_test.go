package route

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

func TestNeverRetriedKinds(t *testing.T) {
	if !nonRetryableKind(inference.UndefinedTool) {
		t.Fatal("undefined tool rejections must be non-retryable")
	}
	if !nonRetryableKind(inference.InvalidProviderResponse) {
		t.Fatal("invalid provider responses must stay non-retryable")
	}
	if !nonRetryableKind(inference.ProviderTruncated) {
		t.Fatal("provider truncations must stay non-retryable")
	}
	if !nonRetryableKind(inference.ProviderResponseUnusable) {
		t.Fatal("completed-but-unusable responses must stay non-retryable")
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
		inference.ProviderTruncated, model.OperationGenerate, "",
		errors.New("stream ended without a finish reason"))
	decision := RetryDecision{
		Operation: model.OperationGenerate,
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

// TestProviderResponseUnusableNeverDefaultRetryable locks in the same D1
// contract for completed-but-unusable responses: the class is attempt-void
// at caller granularity, so core never retries it — not under
// DefaultRetryable, and not even when retryable_unavailable is configured.
func TestProviderResponseUnusableNeverDefaultRetryable(t *testing.T) {
	infErr := inference.NewError(
		inference.ProviderResponseUnusable, model.OperationGenerate, "",
		errors.New("completed response rejected by the request contract"))
	decision := RetryDecision{
		Operation: model.OperationGenerate,
		Phase:     AttemptPhaseExecute,
		ErrorKind: inference.ProviderResponseUnusable,
		Err:       infErr,
		Attempt:   0,
	}
	if DefaultRetryable(context.Background(), decision) {
		t.Fatal("provider_response_unusable must never be retried by DefaultRetryable")
	}
	policy, err := (&RetryPolicyConfig{
		MaxAttempts: 2,
		Retryable:   []RetryableClass{RetryableUnavailable},
	}).policy()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if retryEligible(context.Background(), policy, decision) {
		t.Fatal("provider_response_unusable must not be retried even when retryable_unavailable is configured")
	}
}
