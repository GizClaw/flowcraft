package route

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// TestPolicyTablesCoverOperationVocabulary is the guard that keeps the
// operation axis from drifting. Every table the route policy is written
// against must cover model.Operations(); adding a workload without wiring a
// table fails here instead of silently skipping that workload at runtime —
// which is exactly how transcription pools escaped Policy.ValidateFor.
func TestPolicyTablesCoverOperationVocabulary(t *testing.T) {
	policy := Policy{}
	pools := map[model.Operation]bool{}
	for _, entry := range policy.poolsByOperation() {
		if pools[entry.operation] {
			t.Errorf("poolsByOperation lists %q twice", entry.operation)
		}
		pools[entry.operation] = true
	}
	retryConfig := map[model.Operation]bool{}
	for _, entry := range (RetryConfig{}).byOperation() {
		if retryConfig[entry.operation] {
			t.Errorf("RetryConfig.byOperation lists %q twice", entry.operation)
		}
		retryConfig[entry.operation] = true
	}
	retryPolicies := map[model.Operation]bool{}
	for _, entry := range (RetryPolicies{}).byOperation() {
		if retryPolicies[entry.operation] {
			t.Errorf("RetryPolicies.byOperation lists %q twice", entry.operation)
		}
		retryPolicies[entry.operation] = true
	}
	for _, operation := range model.Operations() {
		if !pools[operation] {
			t.Errorf("poolsByOperation misses %q: its pools would never be validated", operation)
		}
		if !retryConfig[operation] {
			t.Errorf("RetryConfig.byOperation misses %q: a retry section for it would be ignored", operation)
		}
		if !retryPolicies[operation] {
			t.Errorf("RetryPolicies.byOperation misses %q: retry would silently not apply", operation)
		}
	}
}

// TestValidateForChecksTranscriptionTargets is the regression test for the
// build-time validation gap: a transcription pool naming a provider that does
// not exist used to pass ValidateFor and fail only at call time.
func TestValidateForChecksTranscriptionTargets(t *testing.T) {
	assembly := newRouteAssembly(t)
	policy := Policy{Transcription: []Pool{{
		Tier: "asr",
		Targets: []Target{{
			Model: model.ModelRef{
				ID: model.ModelID{Provider: "missing", Name: "ghost"},
			},
		}},
	}}}
	err := policy.ValidateFor(assembly)
	if err == nil {
		t.Fatal("ValidateFor accepted a transcription target that does not exist")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("ValidateFor error does not name the bad target: %v", err)
	}
}

// TestValidateForChecksTranscriptionOperation pins the second half of the
// check: the target must exist *and* expose the operation. The route assembly
// fixtures only serve generate, so every transcription target here is
// operation-less.
func TestValidateForChecksTranscriptionOperation(t *testing.T) {
	assembly := newRouteAssembly(t)
	policy := Policy{Transcription: []Pool{{
		Tier: "asr",
		Targets: []Target{{
			Model: model.ModelRef{
				ID: model.ModelID{Provider: "good", Name: "model-1"},
			},
		}},
	}}}
	err := policy.ValidateFor(assembly)
	if err == nil {
		t.Fatal("ValidateFor accepted a target without the transcription operation")
	}
	if !strings.Contains(err.Error(), "does not expose the operation") {
		t.Fatalf("ValidateFor error does not name the missing operation: %v", err)
	}
}

// TestRetryPolicyLookupCoversVocabulary pins the runtime lookup against the
// same table: a configured policy must be reachable through policyFor.
func TestRetryPolicyLookupCoversVocabulary(t *testing.T) {
	policies := RetryPolicies{
		Generate:      &RetryPolicy{MaxAttempts: 2},
		Embed:         &RetryPolicy{MaxAttempts: 2},
		Transcription: &RetryPolicy{MaxAttempts: 2},
	}
	for _, operation := range model.Operations() {
		if policies.policyFor(operation) == nil {
			t.Errorf("policyFor(%q) = nil, want the configured policy", operation)
		}
	}
	if policies.policyFor(model.Operation("unlisted")) != nil {
		t.Error("policyFor returned a policy for an operation outside the vocabulary")
	}
}
