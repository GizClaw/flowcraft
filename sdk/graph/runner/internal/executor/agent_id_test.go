package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// TestAgentIDFor_AttributesWinOverCtxKey pins the precedence
// documented on agentIDFor: the canonical wire key
// (cfg.attributes[telemetry.AttrAgentID], populated upstream by
// agent.Run.mergeAttributes) takes priority over the legacy
// WithActorKey ctx-key. This is the rule that lets agent.Run
// stamp the envelope agent id without callers having to manually
// thread WithActorKey through the executor entry point.
func TestAgentIDFor_AttributesWinOverCtxKey(t *testing.T) {
	ctx := WithActorKey(context.Background(), "ctx-actor")
	cfg := runConfig{attributes: map[string]string{
		telemetry.AttrAgentID: "attr-agent",
	}}
	if got := agentIDFor(ctx, cfg); got != "attr-agent" {
		t.Fatalf("attributes should win: got %q want %q", got, "attr-agent")
	}
}

// TestAgentIDFor_FallsBackToCtxKeyWhenAttributesEmpty asserts the
// back-compat path: legacy callers that drove the executor with
// WithActorKey (no agent.Run on top) still see their identifier on
// the envelope after the migration. Without this the migration
// would silently strip agent_id from existing pipelines until
// they switched to the attribute-based seed.
func TestAgentIDFor_FallsBackToCtxKeyWhenAttributesEmpty(t *testing.T) {
	ctx := WithActorKey(context.Background(), "ctx-actor")

	if got := agentIDFor(ctx, runConfig{}); got != "ctx-actor" {
		t.Errorf("nil attributes: got %q want %q", got, "ctx-actor")
	}
	cfg := runConfig{attributes: map[string]string{"unrelated": "x"}}
	if got := agentIDFor(ctx, cfg); got != "ctx-actor" {
		t.Errorf("missing AttrAgentID: got %q want %q", got, "ctx-actor")
	}
	cfg = runConfig{attributes: map[string]string{telemetry.AttrAgentID: ""}}
	if got := agentIDFor(ctx, cfg); got != "ctx-actor" {
		t.Errorf("empty AttrAgentID should not suppress fallback: got %q", got)
	}
}

// TestAgentIDFor_EmptyWhenBothMissing documents the no-agent case:
// publish helpers skip SetAgentID when the resolved id is empty so
// envelope headers stay clean (no "agent_id":"" pollution) and
// step subjects degrade to the bare nodeID rather than an
// "<empty>.node.<id>" form.
func TestAgentIDFor_EmptyWhenBothMissing(t *testing.T) {
	if got := agentIDFor(context.Background(), runConfig{}); got != "" {
		t.Fatalf("expected empty agent id, got %q", got)
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
		{"node only (legacy ctx-key absent)", "", "n1", "n1"},
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
		// Legacy mirror also populated by SetAgentID dual-write —
		// observers that haven't migrated still see actor_id.
		if got := env.Headers[event.HeaderActorID]; got != "researcher" {
			t.Errorf("envelope %s: legacy actor_id mirror = %q, want %q",
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

// TestExecute_StampsAgentIDFromCtxKeyFallback exercises the legacy
// path. With the attribute bag empty the executor MUST still honour
// WithActorKey-supplied ids until v0.5.0 removal; otherwise existing
// direct-executor callers (tests, embedded users) would silently
// lose envelope agent_id at this version bump.
func TestExecute_StampsAgentIDFromCtxKeyFallback(t *testing.T) {
	host := enginetest.NewMockHost()

	probe := newTestNode("probe", func(_ graph.ExecutionContext, _ *graph.Board) error {
		return nil
	})
	g := buildGraph("agent-fallback", "probe",
		map[string]graph.Node{"probe": probe},
		[]graph.Edge{{From: "probe", To: graph.END}},
	)

	ctx := WithActorKey(context.Background(), "legacy-agent")
	_, err := NewLocalExecutor().Execute(ctx, g, graph.NewBoard(),
		WithRunID("run-fallback"),
		WithHost(host),
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	for _, env := range host.Envelopes() {
		if got := env.AgentID(); got != "legacy-agent" {
			t.Errorf("envelope %s: AgentID = %q, want %q",
				env.Subject, got, "legacy-agent")
		}
	}
}

// Compile-time check that enginetest.MockHost satisfies engine.Host
// (used by the integration tests above).
var _ engine.Host = (*enginetest.MockHost)(nil)
