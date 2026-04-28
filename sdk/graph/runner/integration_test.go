package runner_test

// End-to-end integration of agent.Run + runner.Runner.
//
// Until this file exists every agent_test exercises engine.EngineFunc
// stubs — meaning the agent + engine + graph integration is only
// covered mock-side. The cases below stand up a real graph runner and
// drive it through agent.Run, which is the path production callers
// will take in v0.3.0. They live in the runner package (not agent)
// because the test brings up a real graph + node.Factory, which would
// pull graph imports into the agent test binary and make agent's tests
// transitively depend on the whole graph subtree.

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// echoNode appends an assistant reply to MainChannel quoting the last
// user message. It exercises both inbound (BoardSeeder placed the
// user message) and outbound (assistant reply pickup by
// agent.newAssistantMessages) board flow without needing a real LLM.
type echoNode struct{ id string }

func (n *echoNode) ID() string   { return n.id }
func (n *echoNode) Type() string { return "echo" }
func (n *echoNode) ExecuteBoard(_ graph.ExecutionContext, b *graph.Board) error {
	main := b.Channel(engine.MainChannel)
	if len(main) == 0 {
		return nil
	}
	last := main[len(main)-1]
	reply := model.NewTextMessage(model.RoleAssistant, "echo: "+last.Content())
	b.AppendChannelMessage(engine.MainChannel, reply)
	return nil
}

// buildEchoRunner constructs the trivial echo graph + runner used by
// most cases below. Centralising it keeps each test focused on the
// assertion that varies (status, host plumbing, interrupt
// classification, ...) rather than repeating wiring boilerplate.
func buildEchoRunner(t *testing.T) *runner.Runner {
	t.Helper()

	factory := node.NewFactory()
	factory.RegisterBuilder("echo", func(def graph.NodeDefinition) (graph.Node, error) {
		return &echoNode{id: def.ID}, nil
	})

	def := &graph.GraphDefinition{
		Name:  "echo",
		Entry: "echo",
		Nodes: []graph.NodeDefinition{{ID: "echo", Type: "echo"}},
		Edges: []graph.EdgeDefinition{{From: "echo", To: graph.END}},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// TestAgentRun_HappyPath proves that agent.Run can drive
// runner.Runner end-to-end. This is the core integration the v0.3
// runtime promise relies on; if it ever breaks the agent + graph
// migration story does too.
func TestAgentRun_HappyPath(t *testing.T) {
	r := buildEchoRunner(t)

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "echo-agent"},
		r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hello")},
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("Status = %q, want completed", res.Status)
	}
	if got := res.Text(); got != "echo: hello" {
		t.Fatalf("Text = %q, want %q", got, "echo: hello")
	}
	if res.RunID == "" {
		t.Error("expected auto-generated RunID")
	}
}

// TestAgentRun_HostReceivesEnvelopes asserts the host the caller
// installs via WithEngineHost is the same one the runner publishes
// to. Without this, agent.Run would silently swallow every envelope
// and observability-driven hosts (metrics, tracing, HITL) would be
// invisible from the agent layer.
func TestAgentRun_HostReceivesEnvelopes(t *testing.T) {
	r := buildEchoRunner(t)
	host := enginetest.NewMockHost()

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "echo-agent"},
		r,
		agent.Request{
			RunID:   "run-explicit",
			Message: model.NewTextMessage(model.RoleUser, "hi"),
		},
		agent.WithEngineHost(host),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("Status = %q, want completed", res.Status)
	}

	envs := host.Envelopes()
	if len(envs) == 0 {
		t.Fatal("host received no envelopes — runner is not publishing through the agent-supplied host")
	}
	wantPrefix := "graph.run.run-explicit."
	sawStart, sawEnd := false, false
	for _, e := range envs {
		if !strings.HasPrefix(string(e.Subject), wantPrefix) {
			t.Fatalf("envelope %q lacks expected runID prefix %q", string(e.Subject), wantPrefix)
		}
		if strings.HasSuffix(string(e.Subject), ".start") {
			sawStart = true
		}
		if strings.HasSuffix(string(e.Subject), ".end") {
			sawEnd = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("missing lifecycle envelopes (sawStart=%v sawEnd=%v)", sawStart, sawEnd)
	}
}

// TestAgentRun_HostInterrupt confirms that a host-injected interrupt
// routes back to the agent layer as StatusInterrupted with the cause
// preserved. This is the integration path HITL flows (barge-in, user
// cancel, ...) depend on.
func TestAgentRun_HostInterrupt(t *testing.T) {
	// Use an interrupt-aware node that polls host.Interrupts() before
	// doing any work so the cooperative-interrupt path is exercised
	// deterministically.
	factory := node.NewFactory()
	factory.RegisterBuilder("intr", func(def graph.NodeDefinition) (graph.Node, error) {
		return interruptObservingNode{id: def.ID}, nil
	})
	def := &graph.GraphDefinition{
		Name:  "intr",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "intr"}},
		Edges: []graph.EdgeDefinition{{From: "n", To: graph.END}},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	host := enginetest.NewMockHost()
	host.Interrupt(engine.CauseUserInput, "barge-in")

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "intr-agent"}, r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")},
		agent.WithEngineHost(host),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q, want interrupted", res.Status)
	}
	if res.Cause != engine.CauseUserInput {
		t.Errorf("Cause = %q, want CauseUserInput", res.Cause)
	}
	if !errdefs.IsInterrupted(res.Err) {
		t.Errorf("Result.Err must satisfy errdefs.IsInterrupted, got %v", res.Err)
	}
}

// TestAgentRun_RunIDPropagates verifies that req.RunID flows through
// to the runner and ends up on every envelope subject. This is the
// property hosts use to correlate envelopes with the agent run that
// produced them.
func TestAgentRun_RunIDPropagates(t *testing.T) {
	r := buildEchoRunner(t)
	host := enginetest.NewMockHost()

	const runID = "explicit-run-id"
	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "echo"}, r,
		agent.Request{RunID: runID, Message: model.NewTextMessage(model.RoleUser, "x")},
		agent.WithEngineHost(host),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.RunID != runID {
		t.Fatalf("Result.RunID = %q, want %q", res.RunID, runID)
	}

	envs := host.Envelopes()
	if len(envs) == 0 {
		t.Fatal("expected envelopes")
	}
	for _, e := range envs {
		if !strings.Contains(string(e.Subject), runID) {
			t.Fatalf("envelope %q does not carry runID %q", string(e.Subject), runID)
		}
	}
}

// interruptObservingNode is a tiny graph.Node that does the same
// "drain the host interrupt channel before doing any work" dance
// LLMNode performs in production. Defining it here keeps the
// integration test self-contained without leaking a public helper
// for one assertion.
type interruptObservingNode struct{ id string }

func (n interruptObservingNode) ID() string   { return n.id }
func (n interruptObservingNode) Type() string { return "intr" }
func (n interruptObservingNode) ExecuteBoard(ctx graph.ExecutionContext, _ *graph.Board) error {
	if ctx.Host == nil {
		return nil
	}
	select {
	case intr := <-ctx.Host.Interrupts():
		return engine.Interrupted(intr)
	default:
	}
	return nil
}
