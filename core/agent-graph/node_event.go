package graph

import (
	"github.com/GizClaw/flowcraft/core/agent"
)

// NodeName is the payload name the graph puts on the event stream around each
// invocation.
//
// It is the engine's own fact: this node, in this step, is starting or has finished.
// A surface that needs to tell one node's words from another's reads it; a surface
// that only renders speech ignores it.
const NodeName = "graph.node"

// NodePhase says whether an invocation is starting or has finished.
type NodePhase string

const (
	// NodeStarted is emitted before the node sees any input.
	NodeStarted NodePhase = "start"
	// NodeDone is emitted after the node's result is in hand, before its writes apply.
	NodeDone NodePhase = "end"
)

// NodeEvent reports one node invocation.
//
// A step that runs several nodes reports each one: the pair brackets that node's own
// events, so a reader can attribute what it is watching while the step runs. Under a
// concurrent frontier two nodes' events interleave, which is why attribution is exact
// for a one-node step and a timeline rather than an attribution for a wider one.
type NodeEvent struct {
	// Step is the 1-based step ordinal, the same one a state record carries.
	Step int
	// Node is the node id as the document wrote it.
	Node string
	// Kind is the node's kind ("model", "script", ...).
	Kind string
	// Phase is start or end.
	Phase NodePhase
}

// nodeInvokeEvent renders one invocation boundary.
func nodeInvokeEvent(step int, id, kind string, phase NodePhase) agent.Event {
	return agent.EventExtension[NodeEvent]{
		Name: NodeName,
		Value: NodeEvent{
			Step:  step,
			Node:  id,
			Kind:  kind,
			Phase: phase,
		},
	}
}
