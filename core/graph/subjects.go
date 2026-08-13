package graph

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

// stepActorFor maps an agent and node id to its step actor — the middle
// segment of the step and stream-delta subjects built by the agent
// package (agent.SubjectStepStart, agent.SubjectStreamDelta, …).
//
// The actor MUST start with the executing agent id per the core/agent
// subject contract, with ".node.<nodeID>" appended for the graph
// runner's private suffix. The subject builders sanitise the whole
// actor, so the dotted form is collapsed on the wire; consumers that
// need agent-level filtering use the envelope's agent_id header.
func stepActorFor(agentID, nodeID string) string {
	return agentID + ".node." + nodeID
}

// RunEventPayload describes the graph execution bracket.
type RunEventPayload struct {
	Graph string `json:"graph"`
	Error string `json:"error,omitempty"`

	// RequestID is the provider-assigned request identifier carried by
	// the failure, when the error chain exposes one. Empty otherwise.
	RequestID string `json:"request_id,omitempty"`
}

func publishRunEvent(ctx context.Context, host agent.Host, g *Graph, run agent.Run, subject event.Subject, runErr error) error {
	if host == nil {
		return nil
	}
	payload := RunEventPayload{Graph: g.name}
	if runErr != nil {
		payload.Error = runErr.Error()
		if requestID, ok := errdefs.RequestID(runErr); ok {
			payload.RequestID = requestID
		}
	}
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		recordPublishError(ctx, "run", run.Info(), "")
		return err
	}
	env.SetGraphID(g.name)
	env.SetAgentID(run.AgentID)
	env.SetRunID(run.RunID)
	if err := host.Publish(ctx, env); err != nil {
		recordPublishError(ctx, "run", run.Info(), "")
		return err
	}
	return nil
}

// StepEventPayload is the decoded payload shape of the step lifecycle
// envelopes published around every node invocation.
type StepEventPayload struct {
	// NodeID is the node the step belongs to.
	NodeID string `json:"node_id"`

	// Graph is the graph's name, for cross-graph filtering.
	Graph string `json:"graph"`

	// Skipped marks a step-complete envelope for a node whose skip
	// condition fired: it routed without its handler running.
	Skipped bool `json:"skipped,omitempty"`

	// Error carries the failure message on step-error envelopes.
	Error string `json:"error,omitempty"`

	// RequestID is the provider-assigned request identifier carried by
	// the failure, when the error chain exposes one. Empty otherwise.
	RequestID string `json:"request_id,omitempty"`
}

// publishStepStarted / publishStepCompleted / publishStepError emit
// the step lifecycle events bracketing a node invocation. Emission is
// best-effort: a publishing failure never fails the node.
func publishStepStarted(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepStart(info.RunID, stepActorFor(info.AgentID, nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name})
}

func publishStepCompleted(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepComplete(info.RunID, stepActorFor(info.AgentID, nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name})
}

// publishStepSkipped marks a node whose skip condition fired: a
// step-complete envelope with Skipped=true. The node did not run, but
// observers still see the full traversal.
func publishStepSkipped(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepComplete(info.RunID, stepActorFor(info.AgentID, nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name, Skipped: true})
}

func publishStepError(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string, stepErr error) {
	payload := StepEventPayload{NodeID: nodeID, Graph: g.name, Error: stepErr.Error()}
	if requestID, ok := errdefs.RequestID(stepErr); ok {
		payload.RequestID = requestID
	}
	publishStep(ctx, host, agent.SubjectStepError(info.RunID, stepActorFor(info.AgentID, nodeID)),
		info, payload)
}

func publishStep(ctx context.Context, host agent.Host, subject event.Subject, info agent.RunInfo, payload StepEventPayload) {
	if host == nil {
		return
	}
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		recordPublishError(ctx, "step", info, payload.NodeID)
		return
	}
	env.SetNodeID(payload.NodeID)
	env.SetGraphID(payload.Graph)
	env.SetAgentID(info.AgentID)
	env.SetRunID(info.RunID)
	if err := host.Publish(ctx, env); err != nil {
		recordPublishError(ctx, "step", info, payload.NodeID)
	}
}

// ParallelWaveEventPayload is the decoded payload shape of the
// parallel fork/join envelopes bracketing a fan-out wave
// (agent.SubjectParallelFork / agent.SubjectParallelJoin). Branch-level
// accept/cancel signals travel as stream deltas instead
// (agent.StreamDeltaParallelBranchAccept / …Cancel), so observers get
// wave lifecycle from these envelopes and branch progress from deltas.
type ParallelWaveEventPayload struct {
	// ForkID correlates the fork and join envelopes of one wave.
	ForkID string `json:"fork_id"`

	// Branches lists the wave's branch node ids, in frontier order.
	Branches []string `json:"branches"`

	// Cancelled lists branch ids deliberately cancelled through the
	// wave's ParallelController; set only on join envelopes.
	Cancelled []string `json:"cancelled,omitempty"`

	// Graph is the graph's name, for cross-graph filtering.
	Graph string `json:"graph"`
}

// publishParallelWave emits a parallel fork/join envelope. Emission
// is best-effort: a publishing failure never fails the wave.
func publishParallelWave(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, subject event.Subject, payload ParallelWaveEventPayload) {
	if host == nil {
		return
	}
	payload.Graph = g.name
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		recordPublishError(ctx, "parallel_wave", info, payload.ForkID)
		return
	}
	env.SetGraphID(payload.Graph)
	env.SetAgentID(info.AgentID)
	env.SetRunID(info.RunID)
	if err := host.Publish(ctx, env); err != nil {
		recordPublishError(ctx, "parallel_wave", info, payload.ForkID)
	}
}

// publishStreamDelta mints a stream-delta envelope for a node —
// subject agent.SubjectStreamDelta(runID, stepActor),
// NodeID/GraphID/AgentID/RunID headers — and forwards it to
// Host.Publish. A nil host (tests) makes it a no-op. This is the
// single place where stream-delta envelopes are assembled; node
// plugins (ExecutionContext.EmitStreamDelta) and the kernel's own
// branch events both go through it.
func publishStreamDelta(ctx context.Context, host agent.Host, info agent.RunInfo, graphID, nodeID string, delta agent.StreamDeltaPayload) error {
	if host == nil {
		return nil
	}
	env, err := event.NewEnvelope(ctx,
		agent.SubjectStreamDelta(info.RunID, stepActorFor(info.AgentID, nodeID)), delta)
	if err != nil {
		return err
	}
	env.SetNodeID(nodeID)
	env.SetGraphID(graphID)
	env.SetAgentID(info.AgentID)
	env.SetRunID(info.RunID)
	return host.Publish(ctx, env)
}

// publishBranchDelta emits a parallel branch accept/cancel stream
// delta (agent.StreamDeltaParallelBranchAccept / …Cancel), letting
// UIs track fan-out waves as they happen. Best-effort: publish
// failures never fail the wave, but they are counted (see
// telemetry.go).
func publishBranchDelta(ctx context.Context, host agent.Host, info agent.RunInfo, graphID, nodeID string, delta agent.StreamDeltaPayload) {
	if err := publishStreamDelta(ctx, host, info, graphID, nodeID, delta); err != nil {
		recordPublishError(ctx, "stream_delta", info, nodeID)
	}
}
