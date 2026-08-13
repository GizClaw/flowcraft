// Package nodes ships the standard node types for graph graphs:
// inference (single-shot LLM generation) and tool (batch tool
// execution); the script node lives in the sibling package
// nodes/script. Each factory returns a graph.NodeType ready for
// graph.RegisterType, with collaborators wired explicitly by the host
// — nothing reads global state.
//
// Each type also has a matching one-line register helper —
// RegisterInference, RegisterTool, script.Register — sharing the
// shape RegisterX(reg, deps) error so hosts opt in à la carte:
//
//	reg := graph.NewRegistry()
//	must(nodes.RegisterInference(reg, inferenceDeps))
//	must(nodes.RegisterTool(reg, dispatcher))
//	must(script.Register(reg, scriptDeps))
//
// The three compose into the classic agent loop on graph topology:
//
//	inference ──(tool_pending_key == true)──▶ tool ──┐
//	    ▲                                            │
//	    └──────────────(next turn)───────────────────┘
//
// inference appends the assistant message (tool_calls included) to its
// channel and flags tool_pending; tool reads the pending calls off the
// channel tail, executes them, and appends one role=tool message —
// which is again a valid tail for inference's next turn.
package nodes
