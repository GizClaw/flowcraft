package graph

// Well-known board variable names written by the graph kernel itself.
// They follow the "__" reservation rule documented on
// agent.MainChannel: user-domain code must not introduce keys with
// that prefix.
const (
	// VarInterruptedNode records the node id that was about to run
	// when a cooperative interrupt fired, so hosts can surface
	// "paused at X" in UIs.
	VarInterruptedNode = "__interrupted_node"

	// VarToolCalls accumulates the tool calls executed during the
	// run, appended by tool-calling node types (e.g. the LLM node)
	// for observability and resume-time auditing.
	VarToolCalls = "__tool_calls"
)
