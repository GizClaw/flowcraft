package graph

// Board variable keys owned by the graph layer.
//
// Only engine-produced keys live here — names written by the executor
// itself as a side effect of running a graph. Node-type-specific keys
// (e.g. summarisation index, previous-message count, the LLM
// transcript channel name) belong in the owning node sub-package.
const (
	// VarInterruptedNode records the ID of the node that returned an
	// engine.Interrupted error, written by the executor before propagating
	// the interrupt to the caller. Used by resume flows to know where to
	// restart.
	VarInterruptedNode = "__interrupted_node"

	// VarToolCalls is the engine-managed slice that mirrors tool_call /
	// tool_result stream events into board state, so resume / inspection
	// flows can see the in-flight tool loop without replaying the stream.
	VarToolCalls = "__tool_calls"
)
