package graph

import (
	"github.com/GizClaw/flowcraft/core/agent"
)

// SteerDroppedName is the payload name the graph puts on the event stream when an input was queued and
// never applied.
//
// It is the counterpart of [agent.Interrupted], which reports an input that did reach the transcript.
// A caller that treats Steer's return value as a promise reads one of the two before the turn ends —
// otherwise the input is a handle with no outcome, which is what this event exists to prevent.
const SteerDroppedName = "graph.steer.dropped"

// SteerDropped reports one queued input that the run ended without applying.
//
// The run finished — or failed, or was canceled — with the input still waiting for a boundary that
// never came. The message is not in the transcript and nothing else will mention it: this is the whole
// record of it, which is why it carries the handle the caller is holding.
type SteerDropped struct {
	// ID is the handle Steer returned for this input.
	ID agent.SteerID
	// Intent is why the input was offered, so a log can tell a redirect from a correction.
	Intent agent.SteerIntent
	// Step is the last step the run reached: the input was still waiting when the run got there.
	Step int
}

// steerDroppedEvent renders one dropped input.
func steerDroppedEvent(dropped SteerDropped) agent.Event {
	return agent.EventExtension[SteerDropped]{Name: SteerDroppedName, Value: dropped}
}
