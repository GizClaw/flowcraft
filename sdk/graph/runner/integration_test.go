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
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/llmnode"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
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
	wantPrefix := string(engine.SubjectPrefix) + "run-explicit."
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

// channelMessagesNode is a minimal graph.Node that exercises the
// v0.3 messages-only-on-channel contract end-to-end through
// runner.Runner. It declares a Required PortTypeMessages output port
// (just like llmnode), satisfies it solely via board.SetChannel (no
// SetVar), and otherwise behaves like echoNode. The combination is
// exactly the path issue #87 broke: executor.runNode invokes
// graph.ValidateOutputs against a PortDeclarable node whose required
// messages output lives on a channel, not a var.
//
// Defining the type here (rather than depending on llmnode) keeps the
// regression structural — any future change to ValidateOutputs is
// caught regardless of whether llmnode happens to be in the binary.
type channelMessagesNode struct{ id string }

func (n channelMessagesNode) ID() string   { return n.id }
func (n channelMessagesNode) Type() string { return "chan-msg" }
func (n channelMessagesNode) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: graph.MainChannel, Type: graph.PortTypeMessages, Required: true},
	}
}
func (n channelMessagesNode) OutputPorts() []graph.Port {
	return []graph.Port{
		{Name: graph.MainChannel, Type: graph.PortTypeMessages, Required: true},
	}
}
func (n channelMessagesNode) ExecuteBoard(_ graph.ExecutionContext, b *graph.Board) error {
	main := b.Channel(engine.MainChannel)
	if len(main) == 0 {
		return nil
	}
	last := main[len(main)-1]
	reply := model.NewTextMessage(model.RoleAssistant, "echo: "+last.Content())
	b.AppendChannelMessage(engine.MainChannel, reply)
	return nil
}

// TestAgentRun_PortDeclarable_MessagesChannelOutput regression-guards
// issue #87. Before the fix (sdk@v0.3.0), driving any PortDeclarable
// node whose required PortTypeMessages output is written via
// board.SetChannel — the exact shape llmnode adopted in v0.3 — through
// runner.Runner produced Result.Status="failed" with
// Result.Err = "missing required output port ... from node", because
// graph.ValidateOutputs only consulted board vars. The existing
// echoNode coverage in this file did not implement PortDeclarable, so
// this code path had no e2e test.
func TestAgentRun_PortDeclarable_MessagesChannelOutput(t *testing.T) {
	factory := node.NewFactory()
	factory.RegisterBuilder("chan-msg", func(def graph.NodeDefinition) (graph.Node, error) {
		return channelMessagesNode{id: def.ID}, nil
	})
	def := &graph.GraphDefinition{
		Name:  "chan-msg",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "chan-msg"}},
		Edges: []graph.EdgeDefinition{{From: "n", To: graph.END}},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "chan-msg-agent"}, r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hello")},
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("Status = %q (Err=%v), want completed — likely a regression of issue #87 (graph.ValidateOutputs not channel-aware for PortTypeMessages)", res.Status, res.Err)
	}
	if got := res.Text(); got != "echo: hello" {
		t.Fatalf("Text = %q, want %q", got, "echo: hello")
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

// TestAgentRun_Revise_LoopAgainstRealGraphRunner is the cross-layer
// E2E for FinalizeDecision.Revise + WithMaxRevise + OnRunRevise +
// Result.Attempts. Existing unit tests in sdk/agent cover the loop
// against an engine.EngineFunc stub; this test goes further and
// drives the loop against a real sdk/graph/runner so any regression
// where revise re-attempts skip the engine, see stale board state,
// double-fire OnRunRevise, or miscount Attempts surfaces here.
//
// Scenario: a graph with one counter node that records each
// invocation. The Decider asks for revise on every BeforeFinalize
// call (always-on); WithMaxRevise(3) caps the loop at 3 engine
// executions total. We assert:
//
//   - Engine ran exactly 3 times — the revise budget was honoured
//     end-to-end through agent.Run → graph runner.
//   - Result.Attempts == 3 — the agent layer accounted for every
//     attempt, not just the last one.
//   - OnRunRevise fired exactly 2 times (between attempts 1→2 and
//     2→3) and the prev result it received reflects the in-progress
//     count, not the final committed count.
//   - Each attempt had a fresh board view (the real runner sees the
//     re-seeded user message every time, not residual state from
//     the previous attempt).
func TestAgentRun_Revise_LoopAgainstRealGraphRunner(t *testing.T) {
	var (
		mu          sync.Mutex
		invocations int
		seenInputs  []string
	)

	factory := node.NewFactory()
	factory.RegisterBuilder("counter", func(def graph.NodeDefinition) (graph.Node, error) {
		id := def.ID
		return reviseCountingNode{
			id: id,
			run: func(_ graph.ExecutionContext, b *graph.Board) error {
				mu.Lock()
				invocations++
				main := b.Channel(engine.MainChannel)
				if len(main) > 0 {
					seenInputs = append(seenInputs, main[len(main)-1].Content())
				}
				mu.Unlock()
				b.AppendChannelMessage(engine.MainChannel,
					model.NewTextMessage(model.RoleAssistant, "ok"))
				return nil
			},
		}, nil
	})

	def := &graph.GraphDefinition{
		Name:  "revise-e2e",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "counter"}},
		Edges: []graph.EdgeDefinition{{From: "n", To: graph.END}},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	dec := &alwaysReviseDecider{reason: "needs revision"}
	obs := &reviseRecordingObserver{}

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "revise-agent"}, r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "draft please")},
		agent.WithDecider(dec),
		agent.WithObserver(obs),
		agent.WithMaxRevise(3),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	mu.Lock()
	gotInvocations := invocations
	gotInputs := append([]string(nil), seenInputs...)
	mu.Unlock()

	if gotInvocations != 3 {
		t.Errorf("engine invocations = %d, want 3 (budget cap not honoured)", gotInvocations)
	}
	if res.Attempts != 3 {
		t.Errorf("Result.Attempts = %d, want 3", res.Attempts)
	}
	if got := obs.reviseCount(); got != 2 {
		t.Errorf("OnRunRevise calls = %d, want 2 (one between each pair of attempts; the final attempt does not revise)", got)
	}
	for i, in := range gotInputs {
		if in != "draft please" {
			t.Errorf("attempt %d saw input %q, want %q (board re-seed broken — revise must re-feed the user message)", i+1, in, "draft please")
		}
	}
	if got := obs.lastNextAttempt(); got != 3 {
		t.Errorf("last OnRunRevise nextAttempt = %d, want 3", got)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("final Status = %q, want StatusCompleted", res.Status)
	}
}

// reviseCountingNode is a graph.Node that delegates to a closure so
// the test can capture per-invocation state without writing a fresh
// node type. Mirrors the testNodeFunc pattern used elsewhere in the
// runner test suite but avoids cross-file coupling.
type reviseCountingNode struct {
	id  string
	run func(graph.ExecutionContext, *graph.Board) error
}

func (n reviseCountingNode) ID() string   { return n.id }
func (n reviseCountingNode) Type() string { return "counter" }
func (n reviseCountingNode) ExecuteBoard(ctx graph.ExecutionContext, b *graph.Board) error {
	return n.run(ctx, b)
}

// alwaysReviseDecider asks for revise on every BeforeFinalize call.
// Combined with WithMaxRevise(N) the loop should run exactly N
// times — the budget is the only stopping condition.
type alwaysReviseDecider struct {
	agent.BaseDecider
	reason string
}

func (d *alwaysReviseDecider) BeforeFinalize(_ context.Context, _ agent.RunInfo, _ *agent.Request, _ *agent.Result) (agent.FinalizeDecision, error) {
	return agent.FinalizeDecision{Revise: true, Reason: d.reason}, nil
}

// reviseRecordingObserver captures every OnRunRevise event. The test
// inspects count + the last next-attempt index to assert the loop
// fired the hook on every transition (and only on transitions).
type reviseRecordingObserver struct {
	agent.BaseObserver
	mu     sync.Mutex
	events []reviseEventRecord
}

type reviseEventRecord struct {
	prevAttempts int
	nextAttempt  int
}

func (o *reviseRecordingObserver) OnRunRevise(_ context.Context, _ agent.RunInfo, prev *agent.Result, next int) {
	o.mu.Lock()
	o.events = append(o.events, reviseEventRecord{
		prevAttempts: prev.Attempts,
		nextAttempt:  next,
	})
	o.mu.Unlock()
}

func (o *reviseRecordingObserver) reviseCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

func (o *reviseRecordingObserver) lastNextAttempt() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		return -1
	}
	return o.events[len(o.events)-1].nextAttempt
}

// ============================================================================
// Vertical E2E: Agent.Tools → agent.Run → graph runner → llmnode
// ----------------------------------------------------------------------------
// These tests wire the full chain landed in this PR (run-context-deps-plumbing):
// agent.Agent.Tools is promoted into engine.Run.Deps[depname.ToolAllowedNames];
// the graph runner forwards Deps into ExecutionContext.Deps; and llmnode
// resolves its tool registry + allow-list from there. If ANY seam in that
// chain regresses, these tests fail before any PR ships.

// captureLLM records the GenerateOptions every llm.Generate{,Stream} call
// receives so the test can extract the tool definitions the model was
// offered. Because GenerateOption is an opaque func(*GenerateOptions),
// extraction works by applying the recorded slice to a fresh
// GenerateOptions and reading back .Tools.
type captureLLM struct {
	mu          sync.Mutex
	lastOptions []llm.GenerateOption
}

func (c *captureLLM) record(opts []llm.GenerateOption) {
	c.mu.Lock()
	c.lastOptions = opts
	c.mu.Unlock()
}

func (c *captureLLM) toolsSeen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var resolved llm.GenerateOptions
	for _, opt := range c.lastOptions {
		opt(&resolved)
	}
	out := make([]string, 0, len(resolved.Tools))
	for _, t := range resolved.Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func (c *captureLLM) Generate(_ context.Context, _ []model.Message, opts ...llm.GenerateOption) (model.Message, model.TokenUsage, error) {
	c.record(opts)
	return model.NewTextMessage(model.RoleAssistant, "ok"), model.TokenUsage{}, nil
}

func (c *captureLLM) GenerateStream(_ context.Context, _ []model.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	c.record(opts)
	return &capturedStream{final: model.NewTextMessage(model.RoleAssistant, "ok")}, nil
}

type capturedStream struct {
	final model.Message
	done  bool
}

func (s *capturedStream) Next() bool                 { return false }
func (s *capturedStream) Current() model.StreamChunk { return model.StreamChunk{} }
func (s *capturedStream) Err() error                 { return nil }
func (s *capturedStream) Close() error               { s.done = true; return nil }
func (s *capturedStream) Message() model.Message     { return s.final }
func (s *capturedStream) Usage() model.Usage         { return model.Usage{} }

type fixedResolver struct{ inst llm.LLM }

func (r *fixedResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) { return r.inst, nil }
func (r *fixedResolver) InvalidateCache(_ ...llm.InvalidateOption)            {}

// noopTool is a tool.Tool the registry can hold but which the LLM never
// actually executes in these tests (the captureLLM returns no tool calls).
// Only the tool's Definition().Name reaches the assertion site.
type noopTool struct{ name string }

func (t *noopTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: "noop"}
}

func (t *noopTool) Execute(_ context.Context, _ string) (string, error) {
	return "", nil
}

// buildLLMRunner registers a single llm node that pulls its registry +
// allow-list from ExecutionContext.Deps (Register's toolReg is nil). The
// node config requests three tools — the test then varies agent.Tools to
// see which subset the LLM actually receives.
func buildLLMRunner(t *testing.T, resolver llm.LLMResolver, requestedToolNames []string) *runner.Runner {
	t.Helper()
	factory := node.NewFactory()
	llmnode.Register(factory, resolver, nil) // nil toolReg → resolved from Deps at runtime

	def := &graph.GraphDefinition{
		Name:  "llm_only",
		Entry: "llm",
		Nodes: []graph.NodeDefinition{
			{ID: "llm", Type: "llm", Config: map[string]any{
				"system_prompt": "you are a test",
				"tool_names":    sliceAsAny(requestedToolNames),
			}},
		},
		Edges: []graph.EdgeDefinition{{From: "llm", To: graph.END}},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

func sliceAsAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// TestAgentRun_AgentToolsActsAsPolicyGate is the vertical E2E that
// verifies the entire chain from contract-audit Epics A+B:
//
//	agent.Agent.Tools                                        (PR commit 4 write)
//	  → agent.Run promotes into engine.Run.Deps[...AllowedNames]
//	  → graph runner forwards Deps into ExecutionContext.Deps   (commit 1)
//	  → llmnode.resolveTools intersects with Config.ToolNames   (commit 3)
//	  → llm.GenerateStream receives only the intersected defs
//
// The node asks for [search, fetch, delete_world] but the agent only
// allows [search, fetch]. delete_world MUST be filtered before the
// model call — proving Agent.Tools is now a real, enforced policy gate
// rather than the cosmetic field the audit flagged (#1).
func TestAgentRun_AgentToolsActsAsPolicyGate(t *testing.T) {
	cap := &captureLLM{}
	resolver := &fixedResolver{inst: cap}

	reg := tool.NewRegistry()
	reg.Register(&noopTool{name: "search"})
	reg.Register(&noopTool{name: "fetch"})
	reg.Register(&noopTool{name: "delete_world"})

	deps := engine.NewDependencies()
	deps.Set(depname.ToolRegistry, reg)

	r := buildLLMRunner(t, resolver,
		[]string{"search", "fetch", "delete_world"})

	ag := agent.Agent{ID: "researcher", Tools: []string{"search", "fetch"}}
	res, err := agent.Run(context.Background(), ag, r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")},
		agent.WithDependencies(deps),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("Status = %q, want completed", res.Status)
	}

	got := cap.toolsSeen()
	want := []string{"fetch", "search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LLM tool defs = %v, want %v\n"+
			"This means Agent.Tools failed to act as the run-level\n"+
			"ceiling somewhere on the chain agent → engine.Run.Deps\n"+
			"→ ExecutionContext.Deps → llmnode.resolveTools.",
			got, want)
	}
}

// TestAgentRun_NoAgentToolsKeepsLegacyBehaviour is the back-compat
// guard: agents that haven't opted into Agent.Tools (the vast
// majority of pre-Epic-A code) MUST continue to see the per-node
// Config.ToolNames as the only filter. Without this guard the new
// "deny-by-default" semantics could quietly break every existing
// graph that relies on node-level tool config.
func TestAgentRun_NoAgentToolsKeepsLegacyBehaviour(t *testing.T) {
	cap := &captureLLM{}
	resolver := &fixedResolver{inst: cap}

	reg := tool.NewRegistry()
	reg.Register(&noopTool{name: "search"})
	reg.Register(&noopTool{name: "fetch"})

	deps := engine.NewDependencies()
	deps.Set(depname.ToolRegistry, reg)

	r := buildLLMRunner(t, resolver, []string{"search", "fetch"})

	ag := agent.Agent{ID: "noop"} // no Tools → no policy gate
	if _, err := agent.Run(context.Background(), ag, r,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")},
		agent.WithDependencies(deps),
	); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	got := cap.toolsSeen()
	want := []string{"fetch", "search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy back-compat broken: got %v want %v "+
			"(agent without Tools should not constrain Config.ToolNames)", got, want)
	}
}
