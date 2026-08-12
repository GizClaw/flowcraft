package graph

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

type memoryBusHost struct {
	agent.NoopHost
	bus *event.MemoryBus
}

func (h memoryBusHost) Publish(ctx context.Context, env event.Envelope) error {
	return h.bus.Publish(ctx, env)
}

func TestExecuteLinearRun(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "x", "set_val": 42}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"message": "hello ${board.x}"}`)},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg)

	board := mustRun(t, g, agent.NewBoard())
	if v, _ := board.GetVar("x"); v != float64(42) {
		t.Fatalf("var x = %v", v)
	}
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 1 || msgs[0].Content.Text() != "hello 42" {
		t.Fatalf("channel = %+v", msgs)
	}
}

func TestExecuteConditionalRouting(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "path", "set_val": "left"}`)},
			{ID: "left", Type: "echo", Config: []byte(`{"set_var": "went", "set_val": "left"}`)},
			{ID: "right", Type: "echo", Config: []byte(`{"set_var": "went", "set_val": "right"}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "left", Condition: `path == "left"`},
			{From: "a", To: "right", Condition: `path == "right"`},
			{From: "left", To: END},
			{From: "right", To: END},
		},
	}
	g := mustBuild(t, def, reg)
	board := mustRun(t, g, agent.NewBoard())
	if v, _ := board.GetVar("went"); v != "left" {
		t.Fatalf("went = %v", v)
	}
}

func TestExecuteSkipCondition(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "skip_me", "set_val": true}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"set_var": "ran", "set_val": true}`),
				SkipCondition: "skip_me == true"},
			{ID: "c", Type: "echo", Config: []byte(`{"set_var": "after", "set_val": true}`)},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: END}},
	}, reg)

	board := mustRun(t, g, agent.NewBoard())
	if _, ok := board.GetVar("ran"); ok {
		t.Fatal("skipped node ran")
	}
	if v, _ := board.GetVar("after"); v != true {
		t.Fatal("skipped node did not route")
	}
}

func TestExecuteMaxIterationsBudget(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "loop",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: "a"}},
	}, reg, WithMaxIterations(5))

	_, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("expected budget-exceeded, got %v", err)
	}
}

func TestExecuteInterrupt(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg)

	board, err := g.Execute(context.Background(), testRun(), newInterruptHost(), agent.NewBoard())
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("expected interrupted, got %v", err)
	}
	if v, _ := board.GetVar(VarInterruptedNode); v == nil {
		t.Fatal("interrupted node not recorded")
	}
}

func TestExecuteContextCausePreservesInterrupt(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
	}, reg)
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput, Detail: "replacement turn"})
	cancel(cause)

	board, err := g.Execute(ctx, testRun(), agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("error = %v, want interrupted cause preserved", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want original cause %v", err, cause)
	}
	if got, _ := board.GetVar(VarInterruptedNode); got != "a" {
		t.Fatalf("interrupted node = %v, want a", got)
	}
}

func TestExecuteNodeRetry(t *testing.T) {
	fails := &atomic.Int32{}
	fails.Store(2)
	reg := NewRegistry()
	if err := RegisterType(reg, "echo", echoNode(fails)); err != nil {
		t.Fatal(err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo", Config: []byte(`{"set_var": "ok", "set_val": true}`)}},
	}, reg, WithMaxNodeRetries(3))

	board := mustRun(t, g, agent.NewBoard())
	if v, _ := board.GetVar("ok"); v != true {
		t.Fatal("retry did not recover")
	}
	if fails.Load() != 0 {
		t.Fatalf("expected 2 failures consumed, left %d", fails.Load())
	}
}

// TestExecuteNodeRetryRestoresBoard proves a failed attempt's writes
// are rolled back before the next attempt: a node that appends a
// message and then fails must not leave the duplicated message behind
// when the retry succeeds.
func TestExecuteNodeRetryRestoresBoard(t *testing.T) {
	var calls atomic.Int32
	reg := NewRegistry()
	err := RegisterType(reg, "flaky-writer", NodeType[struct{}]{
		Meta: Meta{Desc: "appends, fails once, then succeeds"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "partial"))
			if calls.Add(1) == 1 {
				return errors.New("transient boom")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "flaky-writer"}},
	}, reg, WithMaxNodeRetries(1))

	board := mustRun(t, g, agent.NewBoard())
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one failure, one retry)", calls.Load())
	}
	if msgs := board.Channel(agent.MainChannel); len(msgs) != 1 {
		t.Fatalf("channel len = %d, want 1 — the failed attempt's append must be rolled back", len(msgs))
	}
}

// TestExecuteNodeRetryBackoffInterruptible proves the backoff wait
// honours cancellation: a cancelled run returns promptly with the
// context-classified error instead of sleeping out its retry budget.
func TestExecuteNodeRetryBackoffInterruptible(t *testing.T) {
	reg := NewRegistry()
	err := RegisterType(reg, "always-fails", NodeType[struct{}]{
		Meta:    Meta{Desc: "always fails retryably"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return errors.New("boom") },
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "always-fails"}},
	}, reg, WithMaxNodeRetries(3)) // 500+1000+1500ms of backoff ahead

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = g.Execute(ctx, testRun(), agent.NoopHost{}, agent.NewBoard())
	if err == nil {
		t.Fatal("cancelled run must fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v — backoff slept through it", elapsed)
	}
	if !errdefs.IsAborted(err) && !errdefs.IsTimeout(err) {
		t.Fatalf("error = %v, want context-classified", err)
	}
}

func TestExecuteHandlerErrorPropagates(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo", Config: []byte(`{"fail": "boom"}`)}},
	}, reg)

	_, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("handler error not propagated: %v", err)
	}
}

func TestExecuteCheckpointsStamped(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:    "g",
		Version: "v7",
		Entry:   "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "x", "set_val": 1}`)},
			{ID: "b", Type: "echo"},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg)

	host := &checkpointHost{}
	_, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(host.cps) != 2 {
		t.Fatalf("expected 2 wave checkpoints, got %d", len(host.cps))
	}
	last := host.cps[len(host.cps)-1]
	if len(last.Steps) != 1 || last.Steps[0] != "b" || last.Iteration != 2 || last.Board == nil {
		t.Fatalf("last checkpoint = %+v", last)
	}
	if last.SpecVersion != "v7" {
		t.Fatalf("last checkpoint SpecVersion = %q, want v7", last.SpecVersion)
	}
	if v := last.Board.Vars["x"]; v != float64(1) {
		t.Fatalf("checkpoint board lost var: %v", v)
	}
}

func TestExecuteResume(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "x", "set_val": 1}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"set_var": "y", "set_val": 2}`)},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg)

	// Resume from a checkpoint at node a: b should run, a should not
	// re-run (x comes from the checkpoint board, not from re-execution).
	cp := &agent.Checkpoint{
		ExecID: "run-1",
		Steps:  []string{"a"},
		Board: &agent.BoardSnapshot{
			Vars:     map[string]any{"x": float64(1), "a_ran": true},
			Channels: map[string][]message.Message{agent.MainChannel: {}},
		},
	}
	run := testRun()
	run.ResumeFrom = cp
	board, err := g.Execute(context.Background(), run, agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("resume Execute: %v", err)
	}
	if v, _ := board.GetVar("y"); v != float64(2) {
		t.Fatalf("successor node did not run, y=%v", v)
	}
	if v, _ := board.GetVar("a_ran"); v != true {
		t.Fatal("checkpoint board state not restored")
	}
}

func TestExecuteResumeRejectsForeignCheckpoint(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
	}, reg)

	run := testRun()
	run.ResumeFrom = &agent.Checkpoint{
		ExecID: "another-run",
		Steps:  []string{"a"},
		Board:  &agent.BoardSnapshot{Vars: map[string]any{}},
	}
	_, err := g.Execute(context.Background(), run, agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}

	if err := g.CanResume(agent.Checkpoint{Steps: []string{"ghost"}, Board: &agent.BoardSnapshot{}}); !errdefs.IsValidation(err) {
		t.Fatalf("unknown checkpoint node accepted: %v", err)
	}
}

func TestExecuteResumeRejectsEmptyExecID(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
	}, reg)

	run := testRun()
	run.ResumeFrom = &agent.Checkpoint{
		Steps: []string{"a"},
		Board: &agent.BoardSnapshot{Vars: map[string]any{}},
	}
	_, err := g.Execute(context.Background(), run, agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error for empty exec id, got %v", err)
	}
}

func TestGraphCanResume_SpecVersionMismatch(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
	}, reg)

	valid := agent.Checkpoint{
		Steps: []string{"a"},
		Board: &agent.BoardSnapshot{Vars: map[string]any{}},
	}
	if err := g.CanResume(valid); err != nil {
		t.Fatalf("empty SpecVersion must skip drift check: %v", err)
	}

	mismatch := valid
	mismatch.SpecVersion = "other-graph"
	if err := g.CanResume(mismatch); !errdefs.IsNotAvailable(err) {
		t.Fatalf("SpecVersion mismatch = %v, want NotAvailable", err)
	}

	versioned := mustBuild(t, &GraphDefinition{
		Name:    "g",
		Version: "v2",
		Entry:   "a",
		Nodes:   []NodeDefinition{{ID: "a", Type: "echo"}},
	}, reg)
	old := valid
	old.SpecVersion = "v1"
	if err := versioned.CanResume(old); !errdefs.IsNotAvailable(err) {
		t.Fatalf("stale Version = %v, want NotAvailable", err)
	}
	current := valid
	current.SpecVersion = "v2"
	if err := versioned.CanResume(current); err != nil {
		t.Fatalf("matching Version rejected: %v", err)
	}
}

func TestExecuteParallelMerge(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo", Config: []byte(`{"set_var": "shared", "set_val": "from-b", "message": "b-msg"}`)},
			{ID: "c", Type: "echo", Config: []byte(`{"set_var": "shared", "set_val": "from-c", "message": "c-msg"}`)},
			{ID: "d", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "d"}, {From: "c", To: "d"},
			{From: "d", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	board := mustRun(t, g, agent.NewBoard())

	// First write wins: b precedes c in wave order.
	if v, _ := board.GetVar("shared"); v != "from-b" {
		t.Fatalf("first-write-wins violated, shared=%v", v)
	}
	// Both branch messages merged in wave order.
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 || msgs[0].Content.Text() != "b-msg" || msgs[1].Content.Text() != "c-msg" {
		t.Fatalf("merged channel = %+v", msgs)
	}
}

func TestExecuteParallelFailureSkipsMerge(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo", Config: []byte(`{"fail": "branch-boom"}`)},
			{ID: "c", Type: "echo", Config: []byte(`{"set_var": "from_c", "set_val": true}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	board, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if err == nil || !strings.Contains(err.Error(), "branch-boom") {
		t.Fatalf("branch failure not propagated: %v", err)
	}
	if _, ok := board.GetVar("from_c"); ok {
		t.Fatal("merge applied despite branch failure")
	}
}

func TestExecuteWriteRoleEnforced(t *testing.T) {
	nt := echoNode(nil)
	nt.Meta.Writes = append(nt.Meta.Writes, Role{Kind: RoleVar, Name: "must_write", Required: true})
	reg := NewRegistry()
	if err := RegisterType(reg, "echo", nt); err != nil {
		t.Fatal(err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}}, // writes nothing
	}, reg)

	_, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsValidation(err) {
		t.Fatalf("required write not enforced: %v", err)
	}
}

// TestExecuteParallelCancelNode exercises the whole path: a healthy
// branch cancels a blocked sibling through the context-carried
// controller; the wave succeeds, the cancelled branch merges as a
// no-op, and the sibling's writes land.
func TestExecuteParallelCancelNode(t *testing.T) {
	reg := NewRegistry()

	bStarted := make(chan struct{})
	err := RegisterType(reg, "blocker", NodeType[struct{}]{
		Meta: Meta{Desc: "blocks until its context is cancelled"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			close(bStarted)
			<-ec.Context.Done()
			return ec.Context.Err()
		},
	})
	if err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	err = RegisterType(reg, "canceller", NodeType[struct{}]{
		Meta: Meta{Desc: "cancels the sibling branch"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			<-bStarted // ensure the sibling is running, not just queued
			ctrl, ok := ParallelControllerFromContext(ec.Context)
			if !ok {
				return errors.New("no parallel controller on branch context")
			}
			if !ctrl.CancelNode("b", "not needed") {
				return errors.New("CancelNode(b) = false")
			}
			board.SetVar("from_c", true)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register canceller: %v", err)
	}
	err = RegisterType(reg, "noop", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	})
	if err != nil {
		t.Fatalf("register noop: %v", err)
	}

	g := mustBuild(t, &GraphDefinition{
		Name:  "cancel-wave",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "noop"},
			{ID: "b", Type: "blocker"},
			{ID: "c", Type: "canceller"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	board, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("cancelled branch must not fail the wave: %v", err)
	}
	if v, _ := board.GetVar("from_c"); v != true {
		t.Fatal("canceller's write missing — merge must still run")
	}
}

// TestExecuteParallelMaxBranchesExceeded proves a fan-out wave larger
// than MaxBranches fails the run with a budget-classified error
// instead of silently truncating branches.
func TestExecuteParallelMaxBranchesExceeded(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
			{ID: "c", Type: "echo"},
			{ID: "d", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "a", To: "d"},
			{From: "b", To: END}, {From: "c", To: END}, {From: "d", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true, MaxBranches: 2}))

	_, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, agent.NewBoard())
	if err == nil {
		t.Fatal("wave of 3 with max_branches 2 must fail")
	}
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("err = %v, want budget-exceeded classification", err)
	}

	// At or under the cap the same graph runs fine.
	g2 := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg, WithParallel(ParallelConfig{Enabled: true, MaxBranches: 2}))
	mustRun(t, g2, agent.NewBoard())
}

// TestExecuteParallelForkJoinEvents proves the kernel brackets every
// fan-out wave with fork/join envelopes carrying the branch roster —
// the wave-level lifecycle hosts previously had to infer from branch
// step events.
func TestExecuteParallelForkJoinEvents(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
			{ID: "c", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	host := &publishHost{}
	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	forks := decodePayloads[ParallelWaveEventPayload](t, host, agent.SubjectParallelFork("run-1"))
	joins := decodePayloads[ParallelWaveEventPayload](t, host, agent.SubjectParallelJoin("run-1"))
	if len(forks) != 1 || len(joins) != 1 {
		t.Fatalf("forks = %d, joins = %d, want 1 each", len(forks), len(joins))
	}
	if len(forks[0].Branches) != 2 {
		t.Fatalf("fork branches = %v, want [b c]", forks[0].Branches)
	}
	if forks[0].Graph != "g" || joins[0].Graph != "g" {
		t.Fatalf("graph header = %q / %q", forks[0].Graph, joins[0].Graph)
	}
	if forks[0].ForkID == "" || forks[0].ForkID != joins[0].ForkID {
		t.Fatalf("fork/join ForkID mismatch: %q vs %q", forks[0].ForkID, joins[0].ForkID)
	}
	if len(joins[0].Cancelled) != 0 {
		t.Fatalf("join cancelled = %v, want empty", joins[0].Cancelled)
	}

	// Fork must precede join in publish order.
	var forkIdx, joinIdx = -1, -1
	for i, s := range host.subjectsOf() {
		switch s {
		case agent.SubjectParallelFork("run-1"):
			forkIdx = i
		case agent.SubjectParallelJoin("run-1"):
			joinIdx = i
		}
	}
	if forkIdx == -1 || joinIdx == -1 || forkIdx > joinIdx {
		t.Fatalf("fork idx %d, join idx %d — fork must precede join", forkIdx, joinIdx)
	}
}

func TestExecuteParallelForkIDChangesWhenLoopRepeatsWave(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "looping-wave",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
			{ID: "c", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "a", Condition: "__iterations < 4"},
			{From: "b", To: END},
			{From: "c", To: "a", Condition: "__iterations < 4"},
			{From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	host := &publishHost{}
	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	forks := decodePayloads[ParallelWaveEventPayload](t, host, agent.SubjectParallelFork("run-1"))
	if len(forks) != 2 {
		t.Fatalf("forks = %d, want 2", len(forks))
	}
	if forks[0].ForkID == forks[1].ForkID {
		t.Fatalf("loop reused ForkID %q", forks[0].ForkID)
	}
	if forks[0].ForkID != "run-1#attempt-1#iteration-1#b" ||
		forks[1].ForkID != "run-1#attempt-1#iteration-4#b" {
		t.Fatalf("ForkIDs = %q, %q", forks[0].ForkID, forks[1].ForkID)
	}

	resumed := testRun()
	resumed.ResumeFrom = &agent.Checkpoint{
		ExecID:    resumed.RunID,
		Steps:     []string{"a"},
		Iteration: 1,
		Board:     &agent.BoardSnapshot{},
	}
	resumeHost := &publishHost{}
	if _, err := g.Execute(context.Background(), resumed, resumeHost, agent.NewBoard()); err != nil {
		t.Fatalf("resume Execute: %v", err)
	}
	resumedForks := decodePayloads[ParallelWaveEventPayload](
		t, resumeHost, agent.SubjectParallelFork("run-1"))
	if len(resumedForks) != len(forks) {
		t.Fatalf("resumed forks = %d, want %d", len(resumedForks), len(forks))
	}
	for i := range forks {
		if resumedForks[i].ForkID != forks[i].ForkID {
			t.Fatalf("resumed ForkID[%d] = %q, want %q", i, resumedForks[i].ForkID, forks[i].ForkID)
		}
	}

	revised := testRun()
	revised.Attributes = map[string]string{"agent.attempt": "2"}
	revisedHost := &publishHost{}
	if _, err := g.Execute(context.Background(), revised, revisedHost, agent.NewBoard()); err != nil {
		t.Fatalf("revised Execute: %v", err)
	}
	revisedForks := decodePayloads[ParallelWaveEventPayload](
		t, revisedHost, agent.SubjectParallelFork("run-1"))
	if revisedForks[0].ForkID != "run-1#attempt-2#iteration-1#b" {
		t.Fatalf("revised ForkID = %q", revisedForks[0].ForkID)
	}
}

// TestExecuteParallelJoinCancelledList proves the join envelope names
// the branches cancelled through the wave's ParallelController.
func TestExecuteParallelJoinCancelledList(t *testing.T) {
	reg := NewRegistry()
	bStarted := make(chan struct{})
	if err := RegisterType(reg, "blocker", NodeType[struct{}]{
		Meta: Meta{Desc: "blocks until cancelled"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			close(bStarted)
			<-ec.Context.Done()
			return ec.Context.Err()
		},
	}); err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	if err := RegisterType(reg, "canceller", NodeType[struct{}]{
		Meta: Meta{Desc: "cancels the sibling branch"},
		Handler: func(ec ExecutionContext, board *agent.Board, _ struct{}) error {
			<-bStarted
			ctrl, ok := ParallelControllerFromContext(ec.Context)
			if !ok || !ctrl.CancelNode("b", "not needed") {
				return errors.New("CancelNode(b) failed")
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("register canceller: %v", err)
	}
	if err := RegisterType(reg, "noop", NodeType[struct{}]{
		Meta:    Meta{Desc: "entry"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatalf("register noop: %v", err)
	}

	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "noop"},
			{ID: "b", Type: "blocker"},
			{ID: "c", Type: "canceller"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg, WithParallel(ParallelConfig{Enabled: true}))

	host := &publishHost{}
	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	joins := decodePayloads[ParallelWaveEventPayload](t, host, agent.SubjectParallelJoin("run-1"))
	if len(joins) != 1 {
		t.Fatalf("joins = %d, want 1", len(joins))
	}
	if len(joins[0].Cancelled) != 1 || joins[0].Cancelled[0] != "b" {
		t.Fatalf("join cancelled = %v, want [b]", joins[0].Cancelled)
	}
}

// TestWithStartNode covers the debug entry override: the run begins
// mid-graph (prefix never executes), unknown ids fail validation, and
// a resume takes precedence over the override.
func TestWithStartNode(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "start-g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "hit_a", "set_val": true}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"set_var": "hit_b", "set_val": true}`)},
			{ID: "c", Type: "echo", Config: []byte(`{"set_var": "hit_c", "set_val": true}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: END},
		},
	}
	g := mustBuild(t, def, reg)

	// Mid-graph start: a is skipped, b and c run.
	board, err := g.Execute(WithStartNode(context.Background(), "b"),
		testRun(), agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, ok := board.GetVar("hit_a"); ok {
		t.Fatal("prefix node a ran despite the start override")
	}
	for _, key := range []string{"hit_b", "hit_c"} {
		if v, _ := board.GetVar(key); v != true {
			t.Fatalf("%s missing after starting at b", key)
		}
	}

	// Unknown start node: validation.
	if _, err := g.Execute(WithStartNode(context.Background(), "ghost"),
		testRun(), agent.NoopHost{}, agent.NewBoard()); !errdefs.IsValidation(err) {
		t.Fatalf("unknown start node error = %v, want validation-classified", err)
	}

	// Resume wins over the override.
	run := testRun()
	run.ResumeFrom = &agent.Checkpoint{
		ExecID: run.RunID,
		Steps:  []string{"c"},
		Board:  &agent.BoardSnapshot{Vars: map[string]any{}},
	}
	board, err = g.Execute(WithStartNode(context.Background(), "b"),
		run, agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute with resume: %v", err)
	}
	// The resume frontier starts after c (END): nothing runs at all,
	// and the start override must not fire either.
	if _, ok := board.GetVar("hit_b"); ok {
		t.Fatal("start override fired despite resume precedence")
	}
}

// TestExecuteResumeRestoresFullFrontier is the regression test for
// the lost-branch defect: a fan-out wave whose branches diverge
// (b→d, c→e, no join) checkpoints the *whole* wave, so a resume
// after a mid-[d,e]-wave crash rebuilds the frontier from every
// branch's edges. The legacy single-node marker would have followed
// only the last branch's edges and silently dropped d's subgraph.
func TestExecuteResumeRestoresFullFrontier(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "b", Type: "echo"},
			{ID: "c", Type: "echo"},
			{ID: "d", Type: "echo", Config: []byte(`{"set_var": "hit_d", "set_val": true}`)},
			{ID: "e", Type: "echo", Config: []byte(`{"set_var": "hit_e", "set_val": true}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "d"}, {From: "c", To: "e"},
			{From: "d", To: END}, {From: "e", To: END},
		},
	}
	g := mustBuild(t, def, reg)

	// One full run, capturing wave-boundary checkpoints. The [b,c]
	// checkpoint is taken *before* the [d,e] wave runs, so its board
	// has no hit_d/hit_e — exactly the state a crash mid-[d,e]-wave
	// would leave behind.
	host := &checkpointHost{}
	if _, err := g.Execute(context.Background(), testRun(), host, agent.NewBoard()); err != nil {
		t.Fatalf("seed Execute: %v", err)
	}
	var waveCP *agent.Checkpoint
	for i := range host.cps {
		steps := host.cps[i].Steps
		if len(steps) == 2 && steps[0] == "b" && steps[1] == "c" {
			waveCP = &host.cps[i]
		}
	}
	if waveCP == nil {
		t.Fatalf("no [b c] wave checkpoint captured: %+v", host.cps)
	}
	if v := waveCP.Board.Vars["hit_d"]; v != nil {
		t.Fatalf("checkpoint board already has hit_d — bad test setup: %v", v)
	}

	// Resume from that checkpoint: BOTH divergent successors run.
	run := testRun()
	run.ResumeFrom = waveCP
	board, err := g.Execute(context.Background(), run, agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("resume Execute: %v", err)
	}
	if v, _ := board.GetVar("hit_d"); v != true {
		t.Fatal("d (first branch's successor) lost on resume")
	}
	if v, _ := board.GetVar("hit_e"); v != true {
		t.Fatal("e (last branch's successor) lost on resume")
	}
}

// TestExecuteIterationsSoftExit proves the kernel injects the node
// invocation count into edge conditions: a loop back-edge guarded by
// "__iterations < 4" stops looping after 4 invocations and falls
// through to the default END edge — a clean exit, not a budget error.
func TestExecuteIterationsSoftExit(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"message": "a"}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"message": "b"}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"},
			{From: "b", To: "a", Condition: "__iterations < 4"},
			{From: "b", To: END},
		},
	}, reg)

	board := mustRun(t, g, agent.NewBoard())
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 invocations (a,b,a,b), got %d", len(msgs))
	}
	if msgs[3].Content.Text() != "b" {
		t.Fatalf("last invocation = %q, want b", msgs[3].Content.Text())
	}
}

// TestExecuteIterationsShadowsBoardVar proves the kernel-injected
// "__iterations" wins over a same-named board var in edge conditions —
// even a rule-breaking user var cannot fake out a loop's soft exit.
func TestExecuteIterationsShadowsBoardVar(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"message": "a"}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"message": "b"}`)},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b", Condition: "__iterations < 2"},
			{From: "b", To: END},
		},
	}, reg)

	board := agent.NewBoard()
	board.SetVar("__iterations", 999)
	board = mustRun(t, g, board)
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 || msgs[1].Content.Text() != "b" {
		t.Fatalf("user var 999 shadowed kernel counter; channel = %+v", msgs)
	}
}

func TestGraphExecutePublishesRunLifecycleOnSuccessAndFailure(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterType(registry, "run-lifecycle", NodeType[struct{}]{
		Meta:    Meta{Desc: "run lifecycle"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	graph := mustBuild(t, &GraphDefinition{
		Name:  "run-lifecycle",
		Entry: "entry",
		Nodes: []NodeDefinition{{ID: "entry", Type: "run-lifecycle"}},
		Edges: []EdgeDefinition{{From: "entry", To: END}},
	}, registry)

	for _, test := range []struct {
		name string
		run  agent.Run
	}{
		{name: "success", run: testRun()},
		{
			name: "validation failure",
			run: agent.Run{
				Identity: agent.Identity{AgentID: "agent-1", RunID: "run-failed"},
				ResumeFrom: &agent.Checkpoint{
					ExecID: "different-run",
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &publishHost{}
			_, _ = graph.Execute(context.Background(), test.run, host, agent.NewBoard())
			subjects := host.subjectsOf()
			if len(subjects) < 2 {
				t.Fatalf("subjects = %v, want run start and end", subjects)
			}
			if subjects[0] != agent.SubjectRunStart(test.run.RunID) {
				t.Fatalf("first subject = %q, want run start", subjects[0])
			}
			if subjects[len(subjects)-1] != agent.SubjectRunEnd(test.run.RunID) {
				t.Fatalf("last subject = %q, want run end", subjects[len(subjects)-1])
			}
		})
	}

	_, err := graph.Execute(context.Background(), testRun(), &runEndFailHost{}, agent.NewBoard())
	var publishErr *agent.RunEndPublishError
	if !errors.As(err, &publishErr) {
		t.Fatalf("run-end publish error = %v, want RunEndPublishError", err)
	}
	if !errdefs.IsInternal(err) {
		t.Fatalf("run-end publish error = %v, want internal classification", err)
	}
}

func TestGraphExecuteRunEndPublishIsBoundedWhenBlockSubscriberStopsConsuming(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterType(registry, "run-end-timeout", NodeType[struct{}]{
		Meta:    Meta{Desc: "run end timeout"},
		Handler: func(ExecutionContext, *agent.Board, struct{}) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	graph := mustBuild(t, &GraphDefinition{
		Name:  "run-end-timeout",
		Entry: "entry",
		Nodes: []NodeDefinition{{ID: "entry", Type: "run-end-timeout"}},
		Edges: []EdgeDefinition{{From: "entry", To: END}},
	}, registry, WithRunEndPublishTimeout(30*time.Millisecond))

	run := agent.Run{Identity: agent.Identity{AgentID: "agent-1", RunID: "run-blocked-end"}}
	bus := event.NewMemoryBus()
	sub, err := bus.Subscribe(
		context.Background(),
		event.Pattern(agent.SubjectRunEnd(run.RunID)),
		event.WithBufferSize(1),
		event.WithBackpressure(event.Block),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sub.Close()
		_ = bus.Close()
	}()
	if err := bus.Publish(context.Background(), event.Envelope{
		Subject: agent.SubjectRunEnd(run.RunID),
	}); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, executeErr := graph.Execute(context.Background(), run, memoryBusHost{bus: bus}, agent.NewBoard())
		result <- executeErr
	}()

	select {
	case err := <-result:
		var publishErr *agent.RunEndPublishError
		if !errors.As(err, &publishErr) {
			t.Fatalf("Execute error = %v, want RunEndPublishError", err)
		}
		if !errdefs.IsInternal(err) {
			t.Fatalf("Execute error = %v, want internal classification", err)
		}
	case <-time.After(time.Second):
		_ = sub.Close()
		<-result
		t.Fatal("Execute remained blocked publishing run.end")
	}
}
