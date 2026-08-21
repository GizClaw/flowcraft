package telemetry

import "testing"

// TestAttrConstants_StableNames pins the public Attr* constants to
// their on-the-wire string forms. Renaming any of these is a breaking
// change for every dashboard / alert / log query that filters on the
// key — bumping the test forces an explicit, reviewable acknowledgement.
func TestAttrConstants_StableNames(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{AttrAgentID, "agent.id"},
		{AttrTenantID, "tenant.id"},
		{AttrRunID, "run.id"},
		{AttrParentRunID, "parent.run.id"},
		{AttrTaskID, "task.id"},
		{AttrEngineKind, "engine.kind"},
		{AttrRunStatus, "run.status"},
		{AttrGraphName, "graph.name"},
		{AttrNodeID, "node.id"},
		{AttrToolName, "tool.name"},
		{AttrToolCallID, "tool.call_id"},
		{AttrLLMProvider, "llm.provider"},
		{AttrLLMModel, "llm.model"},
		{AttrLLMInputTokens, "llm.tokens.input"},
		{AttrLLMOutputTokens, "llm.tokens.output"},
		{AttrLLMTotalTokens, "llm.tokens.total"},
		{AttrLLMCachedInputTokens, "llm.tokens.input.cached"},
		{AttrLLMCostMicros, "llm.cost.micros"},
		{AttrLLMLatencyMs, "llm.latency.ms"},
		{AttrLLMRequestID, "llm.request.id"},
		{AttrLLMResponseID, "llm.response.id"},
		{AttrConversationID, "conversation.id"},
		{AttrDelegationTarget, "delegation.target"},
		{AttrDelegationMode, "delegation.mode"},
		{AttrDelegationDepth, "delegation.depth"},
		{AttrDelegationCaller, "delegation.caller"},
		{AttrErrorMessage, "error.message"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("attr constant changed: got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestAttrConstants_Unique guards against accidental duplicates across
// the Attr* set: producers expecting different semantics from two keys
// would silently collide if the constants resolved to the same string.
func TestAttrConstants_Unique(t *testing.T) {
	all := []string{
		AttrAgentID, AttrTenantID,
		AttrRunID, AttrParentRunID, AttrTaskID, AttrEngineKind, AttrRunStatus,
		AttrGraphName, AttrNodeID,
		AttrToolName, AttrToolCallID,
		AttrLLMProvider, AttrLLMModel, AttrLLMInputTokens, AttrLLMOutputTokens,
		AttrLLMTotalTokens, AttrLLMCachedInputTokens, AttrLLMCostMicros, AttrLLMLatencyMs,
		AttrLLMRequestID, AttrLLMResponseID,
		AttrConversationID, AttrErrorMessage,
		AttrDelegationTarget, AttrDelegationMode,
		AttrDelegationDepth, AttrDelegationCaller,
	}
	seen := make(map[string]struct{}, len(all))
	for _, k := range all {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate attr constant value: %q", k)
		}
		seen[k] = struct{}{}
	}
}
