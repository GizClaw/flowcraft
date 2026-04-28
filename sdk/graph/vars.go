package graph

// Board variable keys owned by the graph layer.
//
// Two groups live here:
//
//   - Engine-produced keys (VarInterruptedNode, VarToolCalls) — written by the
//     executor as a side effect of graph execution. These are part of graph's
//     own contract and stay here.
//
//   - Chat-application conventions (VarMessages, VarQuery, VarAnswer) — these
//     describe an agent-style chat data model and do NOT belong in the engine
//     layer. They are kept here only as a transitional landing pad while the
//     workflow → agent migration is in progress; scheduled to move to
//     sdk/agent in v0.3.0.
//
// Node-type-specific keys (e.g. summarisation index, previous-message count)
// belong in the owning node sub-package, NOT here.

// --- Engine-produced keys ----------------------------------------------------

const (
	// VarInterruptedNode records the ID of the node that returned
	// graph.ErrInterrupt, written by the executor before propagating the
	// interrupt to the caller. Used by resume flows to know where to restart.
	VarInterruptedNode = "__interrupted_node"

	// VarToolCalls is the engine-managed slice that mirrors tool_call /
	// tool_result stream events into board state, so resume / inspection
	// flows can see the in-flight tool loop without replaying the stream.
	VarToolCalls = "__tool_calls"
)

// --- Chat-application conventions (deprecated) -------------------------------
//
// These keys describe an agent / chat data model and do not belong in the
// engine. They survive here as a transitional shim so existing graph users
// (LLMNode default behaviour, ValidateInputs) keep working while the agent
// runtime is wired up. New code SHOULD pass the messages/query/answer key
// names through node config instead of relying on these constants.

// VarMessages is the canonical board-var key holding a []model.Message
// transcript when graphs do not use the typed message channel.
//
// Deprecated: chat-application convention; will move to sdk/agent in v0.3.0.
// New code should pass messagesKey through node config.
const VarMessages = "messages"

// VarQuery is the canonical key for the user's current question, used by
// LLM/template nodes that fall back to a single-turn query when no
// transcript is available.
//
// Deprecated: chat-application convention; will move to sdk/agent in v0.3.0.
// New code should pass the query key through node config.
const VarQuery = "query"

// VarAnswer is the canonical key for the final assistant answer string
// written by the terminal node before the graph stops.
//
// Deprecated: chat-application convention; will move to sdk/agent in v0.3.0.
// New code should pass the answer key through node config.
const VarAnswer = "answer"
