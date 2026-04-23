package executor

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// Subject convention emitted by this executor:
//
//	graph.run.<runID>.start
//	graph.run.<runID>.end
//	graph.run.<runID>.parallel.fork
//	graph.run.<runID>.parallel.join
//	graph.run.<runID>.node.<nodeID>.start
//	graph.run.<runID>.node.<nodeID>.complete
//	graph.run.<runID>.node.<nodeID>.error
//	graph.run.<runID>.node.<nodeID>.skipped
//	graph.run.<runID>.node.<nodeID>.stream.delta
//
// runID and nodeID values are inserted verbatim. Callers must keep them
// dot-free (validateID below); IDs containing '.', '*' or '>' would
// corrupt the resulting Subject. The executor enforces this by passing
// every id through sanitiseID before constructing a Subject.

const (
	graphSubjectPrefix = "graph.run."
)

// subjGraphStart returns "graph.run.<runID>.start".
func subjGraphStart(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.start", graphSubjectPrefix, sanitiseID(runID)))
}

// subjGraphEnd returns "graph.run.<runID>.end".
func subjGraphEnd(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.end", graphSubjectPrefix, sanitiseID(runID)))
}

// subjParallelFork returns "graph.run.<runID>.parallel.fork".
func subjParallelFork(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.parallel.fork", graphSubjectPrefix, sanitiseID(runID)))
}

// subjParallelJoin returns "graph.run.<runID>.parallel.join".
func subjParallelJoin(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.parallel.join", graphSubjectPrefix, sanitiseID(runID)))
}

// subjNodeStart returns "graph.run.<runID>.node.<nodeID>.start".
func subjNodeStart(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.node.%s.start", graphSubjectPrefix, sanitiseID(runID), sanitiseID(nodeID)))
}

// subjNodeComplete returns "graph.run.<runID>.node.<nodeID>.complete".
func subjNodeComplete(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.node.%s.complete", graphSubjectPrefix, sanitiseID(runID), sanitiseID(nodeID)))
}

// subjNodeError returns "graph.run.<runID>.node.<nodeID>.error".
func subjNodeError(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.node.%s.error", graphSubjectPrefix, sanitiseID(runID), sanitiseID(nodeID)))
}

// subjNodeSkipped returns "graph.run.<runID>.node.<nodeID>.skipped".
func subjNodeSkipped(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.node.%s.skipped", graphSubjectPrefix, sanitiseID(runID), sanitiseID(nodeID)))
}

// subjNodeStreamDelta returns "graph.run.<runID>.node.<nodeID>.stream.delta".
func subjNodeStreamDelta(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.node.%s.stream.delta", graphSubjectPrefix, sanitiseID(runID), sanitiseID(nodeID)))
}

// sanitiseID escapes characters that would corrupt a Subject. Subject
// segments are separated by '.', and '*' / '>' are reserved for Pattern
// wildcards. We replace each occurrence with '_' rather than rejecting
// the input so that a misformed user-supplied runID / nodeID degrades to
// a still-routable subject (over-broad subscriptions still match) instead
// of silently dropping events.
//
// Empty IDs become "_" so the subject keeps a constant segment count.
func sanitiseID(id string) string {
	if id == "" {
		return "_"
	}
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case '.', '*', '>':
			out = append(out, '_')
		default:
			out = append(out, id[i])
		}
	}
	return string(out)
}

// PatternRun returns "graph.run.<runID>.>" — a Pattern that matches every
// event emitted by the executor for the given run.
//
// Exposed for callers that want to subscribe to a specific run without
// hard-coding the subject convention.
func PatternRun(runID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.>", graphSubjectPrefix, sanitiseID(runID)))
}

// PatternAllRuns returns "graph.run.>" — every executor event from any
// run.
func PatternAllRuns() event.Pattern {
	return event.Pattern("graph.run.>")
}

// PatternRunNodes returns "graph.run.<runID>.node.>" — every node-level
// event for the given run.
func PatternRunNodes(runID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.node.>", graphSubjectPrefix, sanitiseID(runID)))
}

// publishGraphEvent fires-and-forgets a graph-level envelope. Headers
// carry the well-known IDs that callers may need for predicate filtering
// when subject routing alone is insufficient (e.g. cross-run aggregations).
//
// Errors from bus.Publish are intentionally swallowed to preserve
// historical behaviour: the executor must not stop graph execution
// because an observer's bus is overloaded.
func publishGraphEvent(ctx context.Context, bus event.Bus, subject event.Subject, runID, graphName, actorKey string, payload any) {
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	if graphName != "" {
		env.SetGraphID(graphName)
	}
	if actorKey != "" {
		env.SetActorID(actorKey)
	}
	_ = bus.Publish(ctx, env)
}

// publishNodeEvent is publishGraphEvent + node_id header.
func publishNodeEvent(ctx context.Context, bus event.Bus, subject event.Subject, runID, graphName, actorKey, nodeID string, payload any) {
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	if graphName != "" {
		env.SetGraphID(graphName)
	}
	if actorKey != "" {
		env.SetActorID(actorKey)
	}
	if nodeID != "" {
		env.SetNodeID(nodeID)
	}
	_ = bus.Publish(ctx, env)
}
