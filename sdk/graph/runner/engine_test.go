package runner_test

import (
	"context"
	"strings"
	"sync"
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
	return r, enginetest.Capabilities{SupportsResume: true}
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

// TestRunner_Execute_PropagatesRunDepsAndAttributes asserts that
// engine.Run.Deps and engine.Run.Attributes flow from the runner
// boundary all the way to ExecutionContext on the node side. This
// closes contract-audit #2 ("engine.Run.Deps had zero readers") at
// the Runner→executor seam: future regressions that drop the
// propagation here would silently break every node that resolves
// dependencies via engine.GetDep instead of builder closures.
func TestRunner_Execute_PropagatesRunDepsAndAttributes(t *testing.T) {
	type registryKey struct{}
	deps := &engine.Dependencies{}
	deps.Set(registryKey{}, "tool-registry-stub")
	attrs := map[string]string{
		"agent.id":   "researcher",
		"task.id":    "task-9",
		"context.id": "thread-3",
	}

	var (
		gotDeps  *engine.Dependencies
		gotAttrs map[string]string
		gotVal   any
		gotOK    bool
	)
	factory := node.NewFactory()
	factory.RegisterBuilder("capture_runctx", func(def graph.NodeDefinition) (graph.Node, error) {
		return testNodeFunc(def.ID, func(ctx graph.ExecutionContext, _ *graph.Board) error {
			gotDeps = ctx.Deps
			gotAttrs = ctx.Attributes
			if ctx.Deps != nil {
				gotVal, gotOK = ctx.Deps.Get(registryKey{})
			}
			return nil
		}), nil
	})

	def := &graph.GraphDefinition{
		Name:  "deps_attrs_propagation",
		Entry: "cap",
		Nodes: []graph.NodeDefinition{{ID: "cap", Type: "capture_runctx"}},
		Edges: []graph.EdgeDefinition{{From: "cap", To: graph.END}},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-prop", Deps: deps, Attributes: attrs},
		enginetest.NewMockHost(), engine.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotDeps != deps {
		t.Errorf("ExecutionContext.Deps: pointer differs from engine.Run.Deps; got %p want %p", gotDeps, deps)
	}
	if !gotOK || gotVal != "tool-registry-stub" {
		t.Errorf("Deps payload missing through propagation: ok=%v val=%v", gotOK, gotVal)
	}
	if len(gotAttrs) != len(attrs) {
		t.Fatalf("ExecutionContext.Attributes len = %d, want %d", len(gotAttrs), len(attrs))
	}
	for k, v := range attrs {
		if gotAttrs[k] != v {
			t.Errorf("Attributes[%q] = %q, want %q", k, gotAttrs[k], v)
		}
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
	wantPrefix := string(engine.SubjectPrefix) + runID + "."
	for _, e := range envs {
		if !strings.HasPrefix(string(e.Subject), wantPrefix) {
			t.Fatalf("envelope subject %q does not carry run.ID prefix %q",
				string(e.Subject), wantPrefix)
		}
	}
}

// TestRunner_Execute_ValidatesResumePreconditions covers the
// admission paths the runner enforces before the executor sees
// ResumeFrom. These are programmer errors and the runner surfaces
// them as errdefs.Validation per the engine.Engine contract.
func TestRunner_Execute_ValidatesResumePreconditions(t *testing.T) {
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
					Step:   "start",
					Board:  engine.NewBoard().Snapshot(),
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if !errdefs.IsValidation(err) {
			t.Fatalf("foreign ExecID must be Validation, got %v", err)
		}
	})

	t.Run("EmptyStep_IsValidation", func(t *testing.T) {
		_, err := r.Execute(context.Background(),
			engine.Run{
				ID: "run-A",
				ResumeFrom: &engine.Checkpoint{
					ExecID: "run-A",
					Board:  engine.NewBoard().Snapshot(),
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if !errdefs.IsValidation(err) {
			t.Fatalf("empty Step must be Validation, got %v", err)
		}
	})

	t.Run("UnknownStep_IsValidation", func(t *testing.T) {
		_, err := r.Execute(context.Background(),
			engine.Run{
				ID: "run-A",
				ResumeFrom: &engine.Checkpoint{
					ExecID: "run-A",
					Step:   "nonexistent_node",
					Board:  engine.NewBoard().Snapshot(),
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if !errdefs.IsValidation(err) {
			t.Fatalf("unknown Step must be Validation, got %v", err)
		}
	})

	t.Run("ForeignGraphName_IsValidation", func(t *testing.T) {
		_, err := r.Execute(context.Background(),
			engine.Run{
				ID: "run-A",
				ResumeFrom: &engine.Checkpoint{
					ExecID:     "run-A",
					Step:       "start",
					Board:      engine.NewBoard().Snapshot(),
					Attributes: map[string]string{"graph_name": "other_graph"},
				},
			},
			enginetest.NewMockHost(), engine.NewBoard())
		if !errdefs.IsValidation(err) {
			t.Fatalf("foreign graph_name must be Validation, got %v", err)
		}
	})
}

// TestRunner_Execute_ResumesFromCheckpoint is the end-to-end
// happy-path: a graph with three nodes is run fully (host captures
// every checkpoint emitted), then a second Execute is invoked with
// ResumeFrom set to the checkpoint taken after node A completed.
// The execution counter asserts that ONLY B and C run the second
// time — A is not re-executed.
func TestRunner_Execute_ResumesFromCheckpoint(t *testing.T) {
	factory := node.NewFactory()
	executions := newExecutionCounter()
	factory.RegisterBuilder("counter", func(def graph.NodeDefinition) (graph.Node, error) {
		return &counterNode{id: def.ID, counter: executions}, nil
	})

	def := &graph.GraphDefinition{
		Name:  "resume_e2e",
		Entry: "A",
		Nodes: []graph.NodeDefinition{
			{ID: "A", Type: "counter"},
			{ID: "B", Type: "counter"},
			{ID: "C", Type: "counter"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "A", To: "B"},
			{From: "B", To: "C"},
			{From: "C", To: graph.END},
		},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	host1 := newCheckpointRecordingHost()
	board1 := engine.NewBoard()
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-1"}, host1, board1); err != nil {
		t.Fatalf("phase 1 Execute: %v", err)
	}
	if executions.count() != 3 {
		t.Fatalf("phase 1 must execute all 3 nodes; got %d", executions.count())
	}

	cps := host1.checkpoints()
	if len(cps) < 1 {
		t.Fatal("phase 1 produced no checkpoints; cannot resume")
	}
	var afterA *engine.Checkpoint
	for i := range cps {
		if cps[i].Step == "A" {
			cp := cps[i]
			afterA = &cp
			break
		}
	}
	if afterA == nil {
		t.Fatalf("phase 1 produced no checkpoint at Step=A; got steps %v", checkpointSteps(cps))
	}

	executions.reset()
	host2 := newCheckpointRecordingHost()
	board2 := engine.NewBoard()
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-1", ResumeFrom: afterA}, host2, board2); err != nil {
		t.Fatalf("phase 2 (resume) Execute: %v", err)
	}

	if executions.count() != 2 {
		t.Errorf("resume must execute only B and C (2 nodes); got %d executions of %v",
			executions.count(), executions.names())
	}
	if names := executions.names(); !equalSlice(names, []string{"B", "C"}) {
		t.Errorf("resume executed %v, want [B C]", names)
	}
}

// TestRunner_Checkpoint_PersistsOriginalStartedAt asserts that the
// executor stamps OriginalStartedAt on every checkpoint it produces
// and that resuming preserves the original value (does not reset
// to the resume's wall-clock time). Dashboards / SLO budget
// trackers depend on this so a long-running resume chain reports
// total wall time rather than the time of the latest replay.
func TestRunner_Checkpoint_PersistsOriginalStartedAt(t *testing.T) {
	factory := node.NewFactory()
	executions := newExecutionCounter()
	factory.RegisterBuilder("counter", func(def graph.NodeDefinition) (graph.Node, error) {
		return &counterNode{id: def.ID, counter: executions}, nil
	})

	def := &graph.GraphDefinition{
		Name:  "started_at_test",
		Entry: "A",
		Nodes: []graph.NodeDefinition{
			{ID: "A", Type: "counter"},
			{ID: "B", Type: "counter"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "A", To: "B"},
			{From: "B", To: graph.END},
		},
	}
	r, err := runner.New(def, factory)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	host1 := newCheckpointRecordingHost()
	startBefore := time.Now()
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-1"}, host1, engine.NewBoard()); err != nil {
		t.Fatalf("phase 1 Execute: %v", err)
	}

	cps := host1.checkpoints()
	if len(cps) == 0 {
		t.Fatal("phase 1 produced no checkpoints")
	}
	for i, cp := range cps {
		if cp.OriginalStartedAt.IsZero() {
			t.Errorf("checkpoint %d has zero OriginalStartedAt", i)
		}
		if cp.OriginalStartedAt.Before(startBefore.Add(-time.Second)) {
			t.Errorf("checkpoint %d OriginalStartedAt %v predates run start %v", i, cp.OriginalStartedAt, startBefore)
		}
	}
	originalStart := cps[0].OriginalStartedAt
	for i, cp := range cps {
		if !cp.OriginalStartedAt.Equal(originalStart) {
			t.Errorf("checkpoint %d OriginalStartedAt drifted: got %v, want %v", i, cp.OriginalStartedAt, originalStart)
		}
	}

	// Phase 2: resume from the after-A checkpoint and assert the
	// new checkpoints carry the SAME OriginalStartedAt as the
	// originating run — not the time the resume was triggered.
	var afterA *engine.Checkpoint
	for i := range cps {
		if cps[i].Step == "A" {
			cp := cps[i]
			afterA = &cp
			break
		}
	}
	if afterA == nil {
		t.Fatalf("no checkpoint at Step=A; got %v", checkpointSteps(cps))
	}

	executions.reset()
	host2 := newCheckpointRecordingHost()
	time.Sleep(5 * time.Millisecond) // make resume wall clock distinct from origin
	if _, err := r.Execute(context.Background(),
		engine.Run{ID: "run-1", ResumeFrom: afterA}, host2, engine.NewBoard()); err != nil {
		t.Fatalf("phase 2 Execute: %v", err)
	}

	resumeCps := host2.checkpoints()
	if len(resumeCps) == 0 {
		t.Fatal("phase 2 produced no checkpoints")
	}
	for i, cp := range resumeCps {
		if !cp.OriginalStartedAt.Equal(originalStart) {
			t.Errorf("resume checkpoint %d OriginalStartedAt = %v, want original %v (resume must inherit, not reset)",
				i, cp.OriginalStartedAt, originalStart)
		}
	}
}

// TestRunner_CanResume_DirectProbe exercises the engine.Resumer
// interface independently of Execute so admin tooling that calls
// CanResume directly (preflight check before scheduling a resume)
// gets the same admission rules.
func TestRunner_CanResume_DirectProbe(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "probe_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{{ID: "start", Type: "passthrough"}},
		Edges: []graph.EdgeDefinition{{From: "start", To: graph.END}},
	}
	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	resumer, ok := engine.AsResumer(r)
	if !ok {
		t.Fatal("Runner does not satisfy engine.Resumer")
	}

	cases := []struct {
		name      string
		cp        engine.Checkpoint
		wantValid bool
	}{
		{"valid", engine.Checkpoint{ExecID: "r", Step: "start"}, true},
		{"valid_with_matching_graph", engine.Checkpoint{ExecID: "r", Step: "start", Attributes: map[string]string{"graph_name": "probe_test"}}, true},
		{"empty_step", engine.Checkpoint{ExecID: "r"}, false},
		{"unknown_step", engine.Checkpoint{ExecID: "r", Step: "ghost"}, false},
		{"foreign_graph", engine.Checkpoint{ExecID: "r", Step: "start", Attributes: map[string]string{"graph_name": "other"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resumer.CanResume(tc.cp)
			if tc.wantValid && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if !tc.wantValid && !errdefs.IsValidation(err) {
				t.Fatalf("expected Validation, got %v", err)
			}
		})
	}
}

// counterNode records the order in which the executor visited it.
// Tests exploit this to assert which nodes were (or were not)
// executed during a resume.
type counterNode struct {
	id      string
	counter *executionCounter
}

func (n *counterNode) ID() string   { return n.id }
func (n *counterNode) Type() string { return "counter" }
func (n *counterNode) ExecuteBoard(_ graph.ExecutionContext, _ *graph.Board) error {
	n.counter.record(n.id)
	return nil
}

type executionCounter struct {
	mu     sync.Mutex
	visits []string
}

func newExecutionCounter() *executionCounter { return &executionCounter{} }

func (c *executionCounter) record(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.visits = append(c.visits, id)
}

func (c *executionCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.visits = nil
}

func (c *executionCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.visits)
}

func (c *executionCounter) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.visits))
	copy(out, c.visits)
	return out
}

// checkpointRecordingHost embeds NoopHost and captures every
// checkpoint the executor publishes via host.Checkpoint. The
// resume test uses captured checkpoints as Execute inputs so the
// scenario mirrors real persistence (store.Save → store.Load) by
// substituting an in-memory recorder.
type checkpointRecordingHost struct {
	engine.NoopHost
	mu sync.Mutex
	cp []engine.Checkpoint
}

func newCheckpointRecordingHost() *checkpointRecordingHost {
	return &checkpointRecordingHost{}
}

func (h *checkpointRecordingHost) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cp = append(h.cp, cp)
	return nil
}

func (h *checkpointRecordingHost) checkpoints() []engine.Checkpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]engine.Checkpoint, len(h.cp))
	copy(out, h.cp)
	return out
}

func checkpointSteps(cps []engine.Checkpoint) []string {
	out := make([]string, len(cps))
	for i, cp := range cps {
		out[i] = cp.Step
	}
	return out
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunner_Capabilities_HonestlyReportsCurrentBehaviour pins the
// Describer claim to the runner's actual behaviour today. Bumping
// any of these flags MUST be paired with the matching Execute
// change, never just a doc edit; the test catches a one-sided
// update at the next test run.
func TestRunner_Capabilities_HonestlyReportsCurrentBehaviour(t *testing.T) {
	def := &graph.GraphDefinition{
		Name:  "caps_test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{{ID: "start", Type: "passthrough"}},
		Edges: []graph.EdgeDefinition{{From: "start", To: graph.END}},
	}
	r, err := runner.New(def, node.NewFactory())
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	got := engine.CapabilitiesOf(r)
	if !got.SupportsResume {
		t.Errorf("SupportsResume = false, but Execute consumes ResumeFrom and Runner implements engine.Resumer — claim is needed")
	}
	if !got.EmitsCheckpoint {
		t.Errorf("EmitsCheckpoint = false, but the executor calls host.Checkpoint after every node — claim is needed")
	}
	if got.EmitsUserPrompt {
		t.Errorf("EmitsUserPrompt = true, but the runner core does not call host.AskUser; only optional plugin nodes do")
	}
	if len(got.RequiredDepNames) != 0 {
		t.Errorf("RequiredDepNames = %v; the runner is a meta-engine and declares no intrinsic deps", got.RequiredDepNames)
	}
	if _, ok := engine.AsResumer(r); !ok {
		t.Errorf("SupportsResume claim requires engine.Resumer implementation; AsResumer returned (nil, false)")
	}
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
