package scriptnode

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/script"
)

// newNodeBridge exposes the current graph node's identity to scripts as
// the global "node". It is a deliberately graph-layer bridge — "node"
// is a graph executor concept that does not exist at the engine /
// agent / bindings layer, so the bridge lives next to ScriptNode
// rather than in sdk/script/bindings.
//
// The split-of-concerns is:
//
//   - "run"  (NewRunInfoBridge in bindings, fed from agent.RunInfo) —
//     per-run identity: run / task / agent / context ids. Immutable
//     across all steps of a single run.
//   - "node" (this bridge) — per-step identity of one graph node.
//     Re-instantiated for every node the executor invokes; visible
//     only to scripts running inside that node.
//   - "host" (NewHostBridge in bindings) — control plane (publish /
//     emit / askUser / ...) wired with the engine.Host and the
//     executor's per-node stream publisher.
//
// Script-facing API:
//
//	node.id()    string  // the graph.NodeDefinition.ID being executed
//	node.type()  string  // the registered node type (e.g. "answer",
//	                       "approval", "iteration", or whatever
//	                       graph.NodeDefinition.Type carried)
//
// Future fields (node.inputs(), node.outputs(), …) should land here
// when scripts demonstrably need them; the bridge stays minimal until
// then.
func newNodeBridge(nodeID, nodeType string) script.BindingFunc {
	return func(_ context.Context) (string, any) {
		return "node", map[string]any{
			"id":   func() string { return nodeID },
			"type": func() string { return nodeType },
		}
	}
}
