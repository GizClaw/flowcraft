package agent

import (
	"fmt"
	"strings"
)

// TraceLevel is how much evidence one run leaves behind.
//
// It lives here, and not in telemetry, because it is a deployment decision rather than an
// instrumentation one: "how much do we record about this agent" is answered where the agent
// is described. The layers that emit spans below this one do not need the enum — the layer
// that assembles their chain does, and it imports this package anyway.
//
// The cost model is per turn of an agent with one model call and two tool calls:
//
//	TraceNone   2 spans   the run and the turn
//	TraceRun    4 spans   + one per driver step
//	TraceCall   7 spans   + the model call and the two tool calls
//	TraceNode   driver-defined; the graph adds a span per node
type TraceLevel int

const (
	// TraceNone records the run and its turns and nothing inside them.
	TraceNone TraceLevel = iota
	// TraceRun adds one span per driver step.
	TraceRun
	// TraceCall adds the model and tool spans. It is the default.
	TraceCall
	// TraceNode adds whatever the driver considers a node of its own.
	TraceNode
)

// String renders the level as the deployment document spells it.
func (l TraceLevel) String() string {
	switch l {
	case TraceNone:
		return "none"
	case TraceRun:
		return "run"
	case TraceCall:
		return "call"
	case TraceNode:
		return "node"
	default:
		return fmt.Sprintf("TraceLevel(%d)", int(l))
	}
}

// ParseTraceLevel reads the document's spelling. An unknown value is an error rather than a
// silent default: a deployment that asked for something else should hear about it at load
// time.
func ParseTraceLevel(value string) (TraceLevel, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "none", "off", "":
		return TraceNone, nil
	case "run":
		return TraceRun, nil
	case "call":
		return TraceCall, nil
	case "node":
		return TraceNode, nil
	default:
		return 0, fmt.Errorf("agent: unknown trace level %q (want none, run, call or node)", value)
	}
}

// AtLeast reports whether l records as much as want. A caller tests the level once and
// skips the whole span-building path when it cannot be used.
func (l TraceLevel) AtLeast(want TraceLevel) bool { return l >= want }
