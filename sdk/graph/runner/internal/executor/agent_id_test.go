package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// TestAgentIDFor_ResolvesFromAttributes pins the single resolution
// source documented on agentIDFor: cfg.attributes[AttrAgentID],
// populated upstream by agent.Run.mergeAttributes and forwarded by
// runner via executor.WithAttributes.
func TestAgentIDFor_ResolvesFromAttributes(t *testing.T) {
	cfg := runConfig{attributes: map[string]string{
		telemetry.AttrAgentID: "attr-agent",
	}}
	if got := agentIDFor(cfg); got != "attr-agent" {
		t.Fatalf("agentIDFor = %q, want %q", got, "attr-agent")
	}
}

// TestAgentIDFor_EmptyWhenAttributeMissing documents the no-agent
// case: publish helpers skip SetAgentID when the resolved id is
// empty so envelope headers stay clean (no "agent_id":""
// pollution) and step subjects degrade to the bare nodeID rather
// than an "<empty>.node.<id>" form.
func TestAgentIDFor_EmptyWhenAttributeMissing(t *testing.T) {
	if got := agentIDFor(runConfig{}); got != "" {
		t.Fatalf("nil attributes: expected empty agent id, got %q", got)
	}
	cfg := runConfig{attributes: map[string]string{"unrelated": "x"}}
	if got := agentIDFor(cfg); got != "" {
		t.Fatalf("missing AttrAgentID: expected empty agent id, got %q", got)
	}
}

// TestStepActorFor pins the contract documented at
// sdk/engine/subjects.go: the stepActor subject segment MUST start
// with the executing agent.id so PatternRunAgentSteps fans-in
// cleanly, and graph runner uses ".node.<nodeID>" as its
// engine-private suffix to disambiguate per-node sub-units.
func TestStepActorFor(t *testing.T) {
	cases := []struct {
		name    string
		agentID string
		nodeID  string
		want    string
	}{
		{"both", "researcher", "n1", "researcher.node.n1"},
		{"agent only (run-level)", "researcher", "", "researcher"},
		{"node only (no agent attribute)", "", "n1", "n1"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepActorFor(tc.agentID, tc.nodeID); got != tc.want {
				t.Errorf("stepActorFor(%q, %q) = %q, want %q", tc.agentID, tc.nodeID, got, tc.want)
			}
		})
	}
}

// TestExecute_StampsAgentIDFromAttributes is the integration-level
// counterpart to the agentIDFor unit test: it drives Execute end-to-
// end and asserts the envelopes published by the executor carry
// HeaderAgentID sourced from cfg.attributes[telemetry.AttrAgentID]
// AND the step subject segment is the compound stepActor
// (= agentID.node.nodeID). Closes contract-audit #15 at the publish
// boundary (where unit-testing agentIDFor alone is not enough — any
// of the four publishGraph/Node call sites could still drop one of
// the dimensions before reaching the wire).
func TestExecute_StampsAgentIDFromAttributes(t *testing.T) {
	host := enginetest.NewMockHost()

	probe := newTestNode("probe", func(_ graph.ExecutionContext, _ *graph.Board) error {
		return nil
	})
	g := buildGraph("agent-test", "probe",
		map[string]graph.Node{"probe": probe},
		[]graph.Edge{{From: "probe", To: graph.END}},
	)

	_, err := NewLocalExecutor().Execute(context.Background(), g, graph.NewBoard(),
		WithRunID("run-agent"),
		WithHost(host),
		WithAttributes(map[string]string{telemetry.AttrAgentID: "researcher"}),
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	envs := host.Envelopes()
	if len(envs) == 0 {
		t.Fatal("host received no envelopes")
	}

	var sawStep bool
	for _, env := range envs {
		// Every envelope MUST carry HeaderAgentID = "researcher".
		// Tolerating "some envelopes missing" would let a regression
		// in any single publish call site slip through.
		if got := env.AgentID(); got != "researcher" {
			t.Errorf("envelope %s: AgentID = %q, want %q",
				env.Subject, got, "researcher")
		}
		// Step subjects MUST carry the compound stepActor segment.
		// Run-level subjects (.start / .end) do not — they have no
		// step actor. SanitiseID collapses the literal "." inside
		// the stepActor (".node.") into "_" so the segment stays
		// one NATS token; agent-level fan-in goes through
		// HeaderAgentID, not subject wildcards (see
		// sdk/engine/subjects.go file header).
		s := string(env.Subject)
		if strings.Contains(s, ".step.") {
			sawStep = true
			if !strings.Contains(s, ".step.researcher_node_probe.") {
				t.Errorf("step subject must contain .step.<agent>_node_<node>.; got %q", s)
			}
		}
	}
	if !sawStep {
		t.Fatal("no step subject published — graph never executed the probe node?")
	}
}

// Compile-time check that enginetest.MockHost satisfies engine.Host
// (used by the integration tests above).
var _ engine.Host = (*enginetest.MockHost)(nil)
