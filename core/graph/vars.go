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

	// VarIterations is the kernel-injected condition environment name
	// holding the number of node invocations executed so far (the same
	// counter WithMaxIterations budgets against; continuous across
	// resume). A loop back-edge can soft-exit with "__iterations < 10".
	// It lives in the reserved "__" namespace (see vars.go); the kernel
	// value shadows any same-named board var as a second line of defense.
	VarIterations = "__iterations"
)
