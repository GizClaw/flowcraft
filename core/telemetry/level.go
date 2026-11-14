package telemetry

import (
	"fmt"
	"strings"
)

// TraceLevel controls how much of a run leaves a trace behind.
//
// It is a deployment/host policy, not a property of the code. The unit it
// governs is "how fine is the record of this run", and it applies to both
// carriers a run produces:
//
//   - spans, which answer "what took long, who called whom";
//   - the durable records an engine keeps beside its state, which answer "which
//     prompt produced this answer, and can it be replayed".
//
// The two are the same ladder because they are the same decision: a run whose
// evidence is per node is a run somebody wants to look at node by node. What the
// ladder does not cover is the resume log — a checkpoint's state writes, its step
// markers and its snapshots — which an engine writes regardless: a checkpoint
// without them is not a checkpoint.
//
// Whether a recorded model call keeps the response's bytes is a separate
// question, and not a level: BODIES is a retention decision (the answer is user
// content, and it is what a replay needs), not a granularity one.
type TraceLevel int

const (
	// TraceOff emits no spans and keeps no evidence. The resume log is still
	// written. This is the zero value, so an unconfigured run pays nothing.
	TraceOff TraceLevel = iota
	// TraceRun emits one span per run.
	TraceRun
	// TraceStep adds one span per frontier.
	TraceStep
	// TraceNode adds one span per node invocation, and is the level at which an
	// engine keeps per-call evidence: the model calls and the effects.
	TraceNode
)

// Allows reports whether a level includes another: a run configured for
// TraceNode also emits the run and step spans.
func (l TraceLevel) Allows(level TraceLevel) bool { return l >= level }

// String renders the level as the deployment document spells it.
func (l TraceLevel) String() string {
	switch l {
	case TraceOff:
		return "off"
	case TraceRun:
		return "run"
	case TraceStep:
		return "step"
	case TraceNode:
		return "node"
	}
	return fmt.Sprintf("TraceLevel(%d)", int(l))
}

// ParseTraceLevel reads the deployment document's spelling of a trace level.
//
// An empty value is off, which is what a zero level already means. A caller that
// must distinguish "not stated" from "stated as off" — a deployment document
// leaves the field out to mean "use the host default" — checks for the empty
// string before calling this, because a default is the host's decision and not a
// parsing rule.
func ParseTraceLevel(value string) (TraceLevel, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return TraceOff, nil
	case "off":
		return TraceOff, nil
	case "run":
		return TraceRun, nil
	case "step":
		return TraceStep, nil
	case "node":
		return TraceNode, nil
	}
	return TraceOff, fmt.Errorf(
		"trace %q is not supported; use one of: off, run, step, node", value)
}
