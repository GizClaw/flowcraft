package graph

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// stepActorFor maps a node id to its step actor — the middle segment
// of the step and stream-delta subjects built by the agent package
// (agent.SubjectStepStart, agent.SubjectStreamDelta, …). The
// "graph_node_" prefix keeps graph actors visually distinct from other
// engine kinds in subject filters.
func stepActorFor(nodeID string) string {
	return "graph_node_" + agent.SanitiseID(nodeID)
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
}

// publishStepStarted / publishStepCompleted / publishStepError emit
// the step lifecycle events bracketing a node invocation. Emission is
// best-effort: a publishing failure never fails the node.
func publishStepStarted(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepStart(info.RunID, stepActorFor(nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name})
}

func publishStepCompleted(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepComplete(info.RunID, stepActorFor(nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name})
}

// publishStepSkipped marks a node whose skip condition fired: a
// step-complete envelope with Skipped=true. The node did not run, but
// observers still see the full traversal.
func publishStepSkipped(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string) {
	publishStep(ctx, host, agent.SubjectStepComplete(info.RunID, stepActorFor(nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name, Skipped: true})
}

func publishStepError(ctx context.Context, host agent.Host, g *Graph, info agent.RunInfo, nodeID string, stepErr error) {
	publishStep(ctx, host, agent.SubjectStepError(info.RunID, stepActorFor(nodeID)),
		info, StepEventPayload{NodeID: nodeID, Graph: g.name, Error: stepErr.Error()})
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
		agent.SubjectStreamDelta(info.RunID, stepActorFor(nodeID)), delta)
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
