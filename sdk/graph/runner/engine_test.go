package runner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
)

// trivialRunnerFactory builds a one-node graph runner that satisfies
// the engine.Engine contract suite. The single node spins on the host
// interrupt channel and returns engine.Interrupted when it fires; this
// is the minimum required to pass the cooperative-interrupt subtests
// without writing a bespoke node implementation per assertion.
func trivialRunnerFactory(t *testing.T) (engine.Engine, enginetest.Capabilities) {
	t.Helper()

	factory := node.NewFactory()
	factory.RegisterBuilder("interrupt_aware", func(def graph.NodeDefinition) (graph.Node, error) {
		return &interruptAwareNode{id: def.ID}, nil
	})

	def := &graph.GraphDefinition{
		Name:  "trivial",
		Entry: "n",
		Nodes: []graph.NodeDefinition{
			{ID: "n", Type: "interrupt_aware"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "n", To: graph.END},
		},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, enginetest.Capabilities{} // resume not implemented yet
}

// interruptAwareNode is the minimum node shape the engine.Engine
// contract suite needs: it observes ctx.Host.Interrupts() so the
// cooperative-interrupt subtest sees an interrupt classification, and
// otherwise returns immediately so the clean-completion subtest passes.
type interruptAwareNode struct{ id string }

func (n *interruptAwareNode) ID() string   { return n.id }
func (n *interruptAwareNode) Type() string { return "interrupt_aware" }
func (n *interruptAwareNode) ExecuteBoard(ctx graph.ExecutionContext, _ *graph.Board) error {
	if ctx.Host == nil {
		return nil
	}
	// Drain any already-queued interrupt before starting work so the
	// suite's "host.Interrupt() then Execute()" ordering surfaces as
	// an Interrupted error. The default branch keeps clean runs
	// non-blocking.
	select {
	case intr := <-ctx.Host.Interrupts():
		return engine.Interrupted(intr)
	default:
		return nil
	}
}

// TestRunner_EngineContract drives the full enginetest.RunSuite
// against runner.Runner. This is the canonical "the graph runner is a
// real engine.Engine" assertion — every subtest covers a clause in
// the contract documented at sdk/engine/engine.go (clean completion,
// ctx cancel, cooperative interrupt, attribute preservation, publish
// error tolerance, resume classification).
func TestRunner_EngineContract(t *testing.T) {
	enginetest.RunSuite(t, func() (engine.Engine, enginetest.Capabilities) {
		return trivialRunnerFactory(t)
	})
}

// TestRunner_Execute_HostInjection asserts that the host parameter
// passed to Execute really reaches nodes via ExecutionContext.Host —
// this is the property agent.Run depends on to inject its host into
// graph runs.
func TestRunner_Execute_HostInjection(t *testing.T) {
	var observed engine.Host

	factory := node.NewFactory()
	factory.RegisterBuilder("capture_host", func(def graph.NodeDefinition) (graph.Node, error) {
		return testNodeFunc(def.ID, func(ctx graph.ExecutionContext, _ *graph.Board) error {
			observed = ctx.Host
			return nil
		}), nil
	})

	def := &graph.GraphDefinition{
		Name:  "host_inj",
		Entry: "cap",
		Nodes: []graph.NodeDefinition{{ID: "cap", Type: "capture_host"}},
		Edges: []graph.EdgeDefinition{{From: "cap", To: graph.END}},
	}

	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	host := enginetest.NewMockHost()
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-host"}, host, engine.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if observed != host {
		t.Fatalf("ExecutionContext.Host did not match the host passed to Execute (got %T %p, want %T %p)",
			observed, observed, host, host)
	}
}

// TestRunner_Execute_PublishesUnderRunID asserts that the run.ID
// parameter (and only that — no executor.WithRunID needed) drives
// the executor's lifecycle subjects. Confirms agent.Run's mintRunID
// flows end-to-end without a parallel WithRunID indirection.
func TestRunner_Execute_PublishesUnderRunID(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "subj_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{{ID: "start", Type: "passthrough"}},
		Edges: []graph.EdgeDefinition{{From: "start", To: graph.END}},
	}
	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	host := enginetest.NewMockHost()
	const runID = "run-from-engine-run"
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: runID}, host, engine.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	envs := host.Envelopes()
	if len(envs) == 0 {
		t.Fatal("host received no envelopes")
	}
	wantPrefix := "graph.run." + runID + "."
	for _, e := range envs {
		if !strings.HasPrefix(string(e.Subject), wantPrefix) {
			t.Fatalf("envelope subject %q does not carry run.ID prefix %q",
				string(e.Subject), wantPrefix)
		}
	}
}

// TestRunner_Execute_RejectsResume verifies the Runner returns the
// contractually correct error class for both resume scenarios:
//   - foreign ExecID → Validation
//   - matching ExecID but resume not implemented → NotAvailable
//
// This is what enginetest's resume subtests assert too, but
// duplicating the assertion here keeps the failure mode obvious if a
// future change tries to silently accept resume.
func TestRunner_Execute_RejectsResume(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "resume_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{{ID: "start", Type: "passthrough"}},
		Edges: []graph.EdgeDefinition{{From: "start", To: graph.END}},
	}
	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	t.Run("ForeignExecID_IsValidation", func(t *testing.T) {
		_, err := r.Execute(context.Background(),
			engine.Run{
				ID: "run-A",
				ResumeFrom: &engine.Checkpoint{
					ExecID: "run-B",
					Board:  engine.NewBoard().Snapshot(),
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if err == nil {
			t.Fatal("expected error for foreign ExecID")
		}
		if !errdefs.IsValidation(err) {
			t.Fatalf("foreign ExecID must be Validation, got %v", err)
		}
	})

	t.Run("MatchingExecID_IsNotAvailable", func(t *testing.T) {
		_, err := r.Execute(context.Background(),
			engine.Run{
				ID: "run-A",
				ResumeFrom: &engine.Checkpoint{
					ExecID: "run-A",
					Board:  engine.NewBoard().Snapshot(),
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if err == nil {
			t.Fatal("expected error for unsupported resume")
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("unsupported resume must be NotAvailable, got %v", err)
		}
	})
}

// TestRunner_Execute_HostNilUsesNoop verifies the Runner falls back to
// engine.NoopHost when the caller passes nil. Same robustness guarantee
// the executor itself provides — Runner must not regress that contract.
func TestRunner_Execute_HostNilUsesNoop(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "nil_host",
		Entry: "start",
		Nodes: []graph.NodeDefinition{{ID: "start", Type: "passthrough"}},
		Edges: []graph.EdgeDefinition{{From: "start", To: graph.END}},
	}
	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-nil"}, nil, engine.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestRunner_Option_WithMaxIterations smoke-tests the new
// runner.WithXxx forwarders by configuring an iteration cap that is
// guaranteed to trip on a self-loop graph. If the option is not
// actually plumbed through to the executor the run would loop until
// the executor's default cap (much higher) and the assertion would
// time out.
func TestRunner_Option_WithMaxIterations(t *testing.T) {
	factory := node.NewFactory()
	factory.RegisterBuilder("loop", func(def graph.NodeDefinition) (graph.Node, error) {
		return testNodeFunc(def.ID, func(_ graph.ExecutionContext, _ *graph.Board) error {
			return nil
		}), nil
	})

	def := &graph.GraphDefinition{
		Name:  "loopy",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "loop"}},
		Edges: []graph.EdgeDefinition{
			{From: "n", To: "n"}, // self-loop with no condition
		},
	}

	r, err := runner.New(def, factory, runner.WithMaxIterations(3))
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The cap MUST trip before the timeout fires — otherwise the option
	// is silently being dropped on the runner side. We assert on the
	// executor's "exceeded max iterations" error string explicitly so
	// a future refactor that wraps the error differently still has to
	// keep the cap behaviour itself.
	_, err = r.Execute(ctx, engine.Run{ID: "loop-run"},
		enginetest.NewMockHost(), engine.NewBoard())
	if err == nil {
		t.Fatal("expected max-iterations error; got nil — runner.WithMaxIterations not plumbed to executor")
	}
	if !strings.Contains(err.Error(), "max iterations") {
		t.Fatalf("expected max-iterations error, got %v", err)
	}
}

// testNodeFunc adapts a function to graph.Node, scoped to this test
// file. The runner_test.go variant was for a different shape of test;
// keeping a private helper here avoids cross-file coupling and makes
// each test self-contained.
type testNodeFuncImpl struct {
	id string
	fn func(graph.ExecutionContext, *graph.Board) error
}

func (n *testNodeFuncImpl) ID() string   { return n.id }
func (n *testNodeFuncImpl) Type() string { return "test" }
func (n *testNodeFuncImpl) ExecuteBoard(ctx graph.ExecutionContext, b *graph.Board) error {
	if n.fn == nil {
		return nil
	}
	return n.fn(ctx, b)
}

func testNodeFunc(id string, fn func(graph.ExecutionContext, *graph.Board) error) graph.Node {
	return &testNodeFuncImpl{id: id, fn: fn}
}
