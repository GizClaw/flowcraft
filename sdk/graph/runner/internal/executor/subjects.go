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
//	engine.run.<runID>.start                          — engine.SubjectRunStart
//	engine.run.<runID>.end                            — engine.SubjectRunEnd
//	engine.run.<runID>.step.<stepActor>.start         — engine.SubjectStepStart
//	engine.run.<runID>.step.<stepActor>.complete      — engine.SubjectStepComplete
//	engine.run.<runID>.step.<stepActor>.error         — engine.SubjectStepError
//	engine.run.<runID>.stream.<stepActor>.delta       — engine.SubjectStreamDelta
//
// graph-private extensions (still under engine.run.<runID>. so a single
// engine.PatternRun subscription captures them; their shape is NOT part
// of the engine contract):
//
//	engine.run.<runID>.parallel.fork
//	engine.run.<runID>.parallel.join
//	engine.run.<runID>.step.<stepActor>.skipped
//
// stepActor follows the engine contract documented in
// sdk/engine/subjects.go: it MUST start with the executing agent.id
// so engine.PatternRunAgentSteps wildcards fan-in cleanly. Graph
// runner appends ".node.<nodeID>" as its engine-private suffix
// (built by stepActorFor in executor.go); that suffix is invisible
// to the engine contract — pure-engine consumers route on the
// agent.id prefix, graph-aware consumers split on ".node." or read
// the parallel envelope.HeaderNodeID.
//
// Both runID and stepActor go through engine.SanitiseID inside the
// builders so a value containing '.' / '*' / '>' degrades to
// '_'-substituted segments rather than corrupting the resulting
// Subject.

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
// aggregations). agentID — the executor identity, sourced from
// engine.Run.Attributes[telemetry.AttrAgentID] via agentIDFor — is
// stamped onto HeaderAgentID (and the legacy HeaderActorID via the
// SetAgentID dual-write) so observers can filter by agent without
// inspecting the subject.
//
// Errors from publisher.Publish are intentionally swallowed to preserve
// the historical behaviour: the executor must not stop graph execution
// because an observer is overloaded. The publisher is the executor's
// composed sink (host + optional legacy bus), built once in Execute.
func publishGraphEvent(ctx context.Context, pub engine.Publisher, subject event.Subject, runID, graphName, agentID string, payload any) {
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
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	// Producer kind ("engine") is encoded in the leading Subject
	// segment by SubjectPrefix; the run id is also carried via
	// HeaderRunID. No separate Envelope.Source is needed — see
	// event.ProducerKind.
	_ = pub.Publish(ctx, env)
}

// publishNodeEvent is publishGraphEvent + node_id header. The caller
// passes agent id and node id as two separate dimensions (envelope
// header level); the matching subject built by the caller carries
// the compound stepActor (= agentID.node.nodeID) so subject-routed
// and header-routed subscribers see the same picture.
func publishNodeEvent(ctx context.Context, pub engine.Publisher, subject event.Subject, runID, graphName, agentID, nodeID string, payload any) {
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
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	if nodeID != "" {
		env.SetNodeID(nodeID)
	}
	_ = pub.Publish(ctx, env)
}
