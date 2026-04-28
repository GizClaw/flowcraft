package executor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

func TestLocalExecutor_ParallelForkJoin(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": newTestNode("branch_a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("a_result", "done_a")
				return nil
			}),
			"branch_b": newTestNode("branch_b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("b_result", "done_b")
				return nil
			}),
			"join": newTestNode("join", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("joined", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v := result.GetVarString("a_result"); v != "done_a" {
		t.Fatalf("expected done_a, got %q", v)
	}
	if v := result.GetVarString("b_result"); v != "done_b" {
		t.Fatalf("expected done_b, got %q", v)
	}
	if v, _ := result.GetVar("joined"); v != true {
		t.Fatal("expected joined=true")
	}
}

func TestLocalExecutor_ParallelNamespaceMerge(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("output", "from_a")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("output", "from_b")
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			MaxBranches:   10,
			MergeStrategy: MergeNamespace,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	a := result.GetVarString("__branch_0.output")
	b := result.GetVarString("__branch_1.output")
	if a == "" || b == "" {
		t.Fatalf("expected namespaced vars, got a=%q b=%q", a, b)
	}
}

func TestLocalExecutor_ParallelForkJoin_IndependentResolvers(t *testing.T) {
	var aConfig, bConfig atomic.Value

	nodeA := &configurableTestNode{
		id:     "branch_a",
		config: map[string]any{"label": "${board.query}_a"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			aConfig.Store(cfg["label"])
			b.SetVar("a_done", true)
			return nil
		},
	}
	nodeB := &configurableTestNode{
		id:     "branch_b",
		config: map[string]any{"label": "${board.query}_b"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			bConfig.Store(cfg["label"])
			b.SetVar("b_done", true)
			return nil
		},
	}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start":    graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": nodeA,
			"branch_b": nodeB,
			"join":     graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("query", "test")

	resolver := variable.NewResolver()
	resolver.AddScope("board", board.Vars())

	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithResolver(resolver),
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v, _ := result.GetVar("a_done"); v != true {
		t.Fatal("expected a_done=true")
	}
	if v, _ := result.GetVar("b_done"); v != true {
		t.Fatal("expected b_done=true")
	}
}

func TestLocalExecutor_ParallelForkJoin_BranchError(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"ok": newTestNode("ok", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("ok_done", true)
				return nil
			}),
			"fail": newTestNode("fail", func(_ graph.ExecutionContext, b *graph.Board) error {
				return fmt.Errorf("branch failed")
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "ok"},
			{From: "start", To: "fail"},
			{From: "ok", To: "join"},
			{From: "fail", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			Enabled:     true,
			MaxBranches: 10,
		}),
	)
	if err == nil {
		t.Fatal("expected error from failing branch")
	}
}

func TestLocalExecutor_ErrorOnConflictMerge(t *testing.T) {
	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start": graph.NewPassthroughNode("start", "passthrough"),
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_a")
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("shared", "from_b")
				return nil
			}),
			"join": graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err := exec.Execute(context.Background(), g, board,
		WithParallel(ParallelConfig{
			MaxBranches:   10,
			MergeStrategy: MergeErrorOnConflict,
		}),
	)
	if err == nil {
		t.Fatal("expected conflict error from parallel merge")
	}
}

func TestLocalExecutor_TypedResolution_ParallelBranches(t *testing.T) {
	var aCapturedTemp, bCapturedTemp atomic.Value

	nodeA := &configurableTestNode{
		id:     "branch_a",
		config: map[string]any{"temperature": "${board.temperature}"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			aCapturedTemp.Store(cfg["temperature"])
			b.SetVar("a_done", true)
			return nil
		},
	}
	nodeB := &configurableTestNode{
		id:     "branch_b",
		config: map[string]any{"temperature": "${board.temperature}"},
		execFn: func(_ graph.ExecutionContext, b *graph.Board, cfg map[string]any) error {
			bCapturedTemp.Store(cfg["temperature"])
			b.SetVar("b_done", true)
			return nil
		},
	}

	g := buildGraph("test", "start",
		map[string]graph.Node{
			"start":    graph.NewPassthroughNode("start", "passthrough"),
			"branch_a": nodeA,
			"branch_b": nodeB,
			"join":     graph.NewPassthroughNode("join", "passthrough"),
		},
		[]graph.Edge{
			{From: "start", To: "branch_a"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: "join"},
			{From: "branch_b", To: "join"},
			{From: "join", To: graph.END},
		},
	)

	board := graph.NewBoard()
	board.SetVar("temperature", 0.7)

	resolver := variable.NewResolver()
	exec := NewLocalExecutor()
	result, err := exec.Execute(context.Background(), g, board,
		WithResolver(resolver),
		WithParallel(ParallelConfig{
			Enabled:       true,
			MaxBranches:   10,
			MergeStrategy: MergeLastWins,
		}),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if v, _ := result.GetVar("a_done"); v != true {
		t.Fatal("expected a_done=true")
	}
	if v, _ := result.GetVar("b_done"); v != true {
		t.Fatal("expected b_done=true")
	}

	aTemp, _ := aCapturedTemp.Load().(float64)
	bTemp, _ := bCapturedTemp.Load().(float64)
	if aTemp != 0.7 {
		t.Fatalf("branch_a temperature: expected 0.7, got %v", aCapturedTemp.Load())
	}
	if bTemp != 0.7 {
		t.Fatalf("branch_b temperature: expected 0.7, got %v", bCapturedTemp.Load())
	}

	if nodeA.config["temperature"] != "${board.temperature}" {
		t.Fatalf("branch_a config should be restored, got %v", nodeA.config["temperature"])
	}
	if nodeB.config["temperature"] != "${board.temperature}" {
		t.Fatalf("branch_b config should be restored, got %v", nodeB.config["temperature"])
	}
}
