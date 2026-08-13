package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// minimalContractDef builds the smallest runnable graph: one echo node
// feeding END. It is reused by every contract subtest via a fresh
// factory call.
func minimalContractDef() *GraphDefinition {
	return &GraphDefinition{
		Name:  "contract",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: END}},
	}
}

// TestGraph_EngineContract runs the shared agent.Engine conformance
// suite against the graph runner. Graph supports checkpoint-driven
// resume, so the factory advertises SupportsResume.
func TestGraph_EngineContract(t *testing.T) {
	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		g := mustBuild(t, minimalContractDef(), newTestRegistry(t))
		return g, agenttest.Capabilities{SupportsResume: true}
	})
}

// TestGraph_ResumerContract runs the shared agent.Resumer conformance
// suite against Graph.CanResume.
func TestGraph_ResumerContract(t *testing.T) {
	agenttest.ResumerSuite(t, func() agent.Resumer {
		return mustBuild(t, minimalContractDef(), newTestRegistry(t))
	})
}

// TestGraph_CheckpointSuggesterContract runs the shared
// agent.CheckpointSuggester conformance suite against the graph
// engine's advisory checkpoint suggestion.
func TestGraph_CheckpointSuggesterContract(t *testing.T) {
	agenttest.CheckpointSuggesterSuite(t, func() agent.CheckpointSuggester {
		return mustBuild(t, minimalContractDef(), newTestRegistry(t))
	})
}
