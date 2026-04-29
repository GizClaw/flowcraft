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
		{AttrPodID, "pod.id"},
		{AttrAgentID, "agent.id"},
		{AttrTenantID, "tenant.id"},
		{AttrRunID, "run.id"},
		{AttrParentRunID, "parent.run.id"},
		{AttrEngineKind, "engine.kind"},
		{AttrRunStatus, "run.status"},
		{AttrGraphName, "graph.name"},
		{AttrNodeID, "node.id"},
		{AttrActorID, "actor.id"},
		{AttrToolName, "tool.name"},
		{AttrToolCallID, "tool.call_id"},
		{AttrLLMModel, "llm.model"},
		{AttrLLMInputTokens, "llm.tokens.input"},
		{AttrLLMOutputTokens, "llm.tokens.output"},
		{AttrLLMTotalTokens, "llm.tokens.total"},
		{AttrLLMCostMicros, "llm.cost.micros"},
		{AttrLLMLatencyMs, "llm.latency.ms"},
		{AttrConversationID, "conversation.id"},
		{AttrDatasetID, "dataset.id"},
		{AttrErrorMessage, "error.message"},
		{AttrKanbanCardID, "kanban.card.id"},
		{AttrKanbanCardKind, "kanban.card.kind"},
		{AttrKanbanProducerID, "kanban.producer.id"},
		{AttrKanbanTargetAgentID, "kanban.target.agent.id"},
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
		AttrPodID, AttrAgentID, AttrTenantID,
		AttrRunID, AttrParentRunID, AttrEngineKind, AttrRunStatus,
		AttrGraphName, AttrNodeID, AttrActorID,
		AttrToolName, AttrToolCallID,
		AttrLLMModel, AttrLLMInputTokens, AttrLLMOutputTokens,
		AttrLLMTotalTokens, AttrLLMCostMicros, AttrLLMLatencyMs,
		AttrConversationID, AttrDatasetID, AttrErrorMessage,
		AttrKanbanCardID, AttrKanbanCardKind, AttrKanbanProducerID,
		AttrKanbanTargetAgentID,
	}
	seen := make(map[string]struct{}, len(all))
	for _, k := range all {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate attr constant value: %q", k)
		}
		seen[k] = struct{}{}
	}
}
