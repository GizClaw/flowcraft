package llmnode

// Board variable keys produced or consumed by the LLM node.
const (
	// VarResponse holds the assistant's text response written by terminal LLM nodes.
	VarResponse = "response"

	// VarUsage holds the cumulative TokenUsage for the current node execution.
	VarUsage = "usage"

	// VarToolPending is true when the LLM round produced tool_calls awaiting
	// dispatch and false otherwise.
	VarToolPending = "tool_pending"

	// VarToolOutput captures the aggregated string output of executed tools.
	VarToolOutput = "tool_output"

	// VarPrevMessageCount snapshots how many messages were on the channel
	// before the LLM node ran, so summarisation/continuation logic can detect
	// new turns.
	VarPrevMessageCount = "__prev_message_count"

	// VarSummaryIndex carries an optional summary string injected into the
	// system prompt by long-context strategies.
	VarSummaryIndex = "__summary_index"

	// VarInternalUsage accumulates TokenUsage across LLM rounds within a single
	// graph execution.
	VarInternalUsage = "__usage"
)
