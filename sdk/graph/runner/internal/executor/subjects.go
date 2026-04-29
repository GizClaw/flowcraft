package executor

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// Subject convention emitted by this executor:
//
// Engine-contract subjects (constructed via sdk/engine builders so the
// names stay aligned with every other engine implementation):
//
//	engine.run.<runID>.start                       — engine.SubjectRunStart
//	engine.run.<runID>.end                         — engine.SubjectRunEnd
//	engine.run.<runID>.step.<nodeID>.start         — engine.SubjectStepStart
//	engine.run.<runID>.step.<nodeID>.complete      — engine.SubjectStepComplete
//	engine.run.<runID>.step.<nodeID>.error         — engine.SubjectStepError
//	engine.run.<runID>.stream.<nodeID>.delta       — engine.SubjectStreamDelta
//
// graph-private extensions (still under engine.run.<runID>. so a single
// engine.PatternRun subscription captures them; their shape is NOT part
// of the engine contract):
//
//	engine.run.<runID>.parallel.fork
//	engine.run.<runID>.parallel.join
//	engine.run.<runID>.step.<nodeID>.skipped
//
// graph runner uses node id as the engine "actor" id. Both go through
// engine.SanitiseID inside the builders so a runID / nodeID containing
// '.' / '*' / '>' degrades to '_'-substituted segments rather than
// corrupting the resulting Subject.

// subjParallelFork returns "engine.run.<runID>.parallel.fork".
//
// graph-private: declared here because engine does not standardise
// fan-out semantics. The shared engine.run.<runID>. prefix means
// engine.PatternRun(runID) still picks it up.
func subjParallelFork(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.parallel.fork", engine.SubjectPrefix, engine.SanitiseID(runID)))
}

// subjParallelJoin returns "engine.run.<runID>.parallel.join".
//
// graph-private: see subjParallelFork.
func subjParallelJoin(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.parallel.join", engine.SubjectPrefix, engine.SanitiseID(runID)))
}

// subjNodeSkipped returns "engine.run.<runID>.step.<nodeID>.skipped".
//
// graph-private: declared here because engine does not standardise
// skip semantics. Sits under the engine "step" namespace so
// engine.PatternRunSteps(runID) picks it up alongside the contract's
// step.start / step.complete / step.error events.
func subjNodeSkipped(runID, nodeID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.step.%s.skipped", engine.SubjectPrefix, engine.SanitiseID(runID), engine.SanitiseID(nodeID)))
}

// publishGraphEvent fires-and-forgets a run-level envelope. Headers
// carry the well-known IDs that callers may need for predicate filtering
// when subject routing alone is insufficient (e.g. cross-run
// aggregations).
//
// Errors from publisher.Publish are intentionally swallowed to preserve
// the historical behaviour: the executor must not stop graph execution
// because an observer is overloaded. The publisher is the executor's
// composed sink (host + optional legacy bus), built once in Execute.
func publishGraphEvent(ctx context.Context, pub engine.Publisher, subject event.Subject, runID, graphName, actorKey string, payload any) {
	if pub == nil {
		return
	}
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
	// Producer kind ("engine") is encoded in the leading Subject
	// segment by SubjectPrefix; the run id is also carried via
	// HeaderRunID. No separate Envelope.Source is needed — see
	// event.ProducerKind.
	_ = pub.Publish(ctx, env)
}

// publishNodeEvent is publishGraphEvent + node_id header.
func publishNodeEvent(ctx context.Context, pub engine.Publisher, subject event.Subject, runID, graphName, actorKey, nodeID string, payload any) {
	if pub == nil {
		return
	}
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
	_ = pub.Publish(ctx, env)
}
