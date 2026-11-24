package telemetry

import "testing"

// TestAttrConstants_StableNames pins the public Attr* constants to their
// on-the-wire string forms. Renaming one is a breaking change for every
// dashboard, alert and log query that filters on the key, so the test forces an
// explicit, reviewable acknowledgement.
func TestAttrConstants_StableNames(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{AttrSandboxExitCode, "sandbox.exit_code"},
		{AttrSandboxRunner, "sandbox.runner"},
		{AttrSandboxTimeoutMs, "sandbox.timeout_ms"},
		{AttrErrorMessage, "error.message"},
		{AttrErrorKind, "error.kind"},
		{AttrErrorRetryAfterMs, "error.retry_after_ms"},
		{AttrGenAIOperationName, "gen_ai.operation.name"},
		{AttrGenAIProviderName, "gen_ai.provider.name"},
		{AttrGenAIRequestModel, "gen_ai.request.model"},
		{AttrGenAIResponseModel, "gen_ai.response.model"},
		{AttrGenAIResponseID, "gen_ai.response.id"},
		{AttrGenAIResponseFinishReasons, "gen_ai.response.finish_reasons"},
		{AttrGenAIUsageInputTokens, "gen_ai.usage.input_tokens"},
		{AttrGenAIUsageOutputTokens, "gen_ai.usage.output_tokens"},
		{AttrGenAIUsageCacheReadInputTokens, "gen_ai.usage.cache_read.input_tokens"},
		{AttrInferenceShape, "inference.shape"},
		{AttrInferenceOperation, "inference.operation"},
		{AttrInferenceErrorDetail, "inference.error.detail"},
		{AttrRouteTarget, "route.target"},
		{AttrRouteSelected, "route.selected"},
		{AttrRouteExecuted, "route.executed"},
		{AttrRouteAttempts, "route.attempts"},
		{AttrRouteRetries, "route.retries"},
		{AttrRouteFallbacks, "route.fallbacks"},
		{AttrRouteOutcome, "route.outcome"},
		{AttrRouteBackoffMs, "route.backoff_ms"},
		{AttrRouteAttemptNumber, "route.attempt.number"},
		{AttrLLMCostMicros, "llm.cost.micros"},
		{AttrLLMRequestID, "llm.request.id"},
		{AttrLLMTimeToFirstTokenMs, "llm.time_to_first_token.ms"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("attr constant changed: got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestAttrConstants_Unique guards against accidental duplicates: two keys that
// resolve to the same string would make one fact look like two.
func TestAttrConstants_Unique(t *testing.T) {
	all := []string{
		AttrSandboxExitCode, AttrSandboxRunner, AttrSandboxTimeoutMs,
		AttrErrorMessage, AttrErrorKind, AttrErrorRetryAfterMs,
		AttrGenAIOperationName, AttrGenAIProviderName, AttrGenAIRequestModel,
		AttrGenAIResponseModel, AttrGenAIResponseID, AttrGenAIResponseFinishReasons,
		AttrGenAIUsageInputTokens, AttrGenAIUsageOutputTokens, AttrGenAIUsageCacheReadInputTokens,
		AttrInferenceShape, AttrInferenceOperation, AttrInferenceErrorDetail,
		AttrRouteTarget, AttrRouteSelected, AttrRouteExecuted, AttrRouteAttempts,
		AttrRouteRetries, AttrRouteFallbacks, AttrRouteOutcome, AttrRouteBackoffMs,
		AttrRouteAttemptNumber,
		AttrLLMCostMicros, AttrLLMRequestID, AttrLLMTimeToFirstTokenMs,
	}
	seen := make(map[string]struct{}, len(all))
	for _, k := range all {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate attr constant value: %q", k)
		}
		seen[k] = struct{}{}
	}
}
