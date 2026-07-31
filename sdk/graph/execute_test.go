package graph

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

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
		Name:  "g",
		Entry: "a",
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
	if last.Step != "b" || last.Iteration != 2 || last.Board == nil {
		t.Fatalf("last checkpoint = %+v", last)
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
		Step:   "a",
		Board: &agent.BoardSnapshot{
			Vars:     map[string]any{"x": float64(1), "a_ran": true},
			Channels: map[string][]inference.Message{agent.MainChannel: {}},
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
		Step:   "a",
		Board:  &agent.BoardSnapshot{Vars: map[string]any{}},
	}
	_, err := g.Execute(context.Background(), run, agent.NoopHost{}, agent.NewBoard())
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}

	if err := g.CanResume(agent.Checkpoint{Step: "ghost", Board: &agent.BoardSnapshot{}}); !errdefs.IsValidation(err) {
		t.Fatalf("unknown checkpoint node accepted: %v", err)
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
