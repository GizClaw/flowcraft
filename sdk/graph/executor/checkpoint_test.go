package executor

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func TestLocalExecutor_Checkpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatalf("create checkpoint store: %v", err)
	}

	g := buildGraph("test", "a",
		map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("a_done", true)
				return nil
			}),
			"b": newTestNode("b", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("b_done", true)
				return nil
			}),
		},
		[]graph.Edge{
			{From: "a", To: "b"},
			{From: "b", To: graph.END},
		},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err = exec.Execute(context.Background(), g, board,
		WithCheckpointStore(store))
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	cp, err := store.Load("test", "")
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if cp == nil {
		t.Fatal("expected checkpoint to exist")
	}
	if cp.NodeID != "b" {
		t.Fatalf("expected last checkpoint at node 'b', got %q", cp.NodeID)
	}
}

func TestFileCheckpointStore_ListSkipsBackups(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"flow_a", "flow_b"} {
		if err := store.Save(Checkpoint{
			GraphName: name,
			NodeID:    "start",
			Board:     graph.NewBoard().Snapshot(),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(Checkpoint{
		GraphName: "flow_a",
		NodeID:    "end",
		Board:     graph.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	names, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("List() returned %d names, want 2: %v", len(names), names)
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["flow_a"] || !nameSet["flow_b"] {
		t.Fatalf("List() = %v, want [flow_a, flow_b]", names)
	}
}

func TestFileCheckpointStore_ListEmptyDir(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("List() on empty dir returned %d names", len(names))
	}
}

func TestFileCheckpointStore_DeleteRemovesAllFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := store.Save(Checkpoint{
			GraphName: "deletable",
			NodeID:    "n",
			Board:     graph.NewBoard().Snapshot(),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	names, _ := store.List()
	if len(names) != 1 || names[0] != "deletable" {
		t.Fatalf("pre-delete List() = %v", names)
	}

	if err := store.Delete("deletable"); err != nil {
		t.Fatal(err)
	}

	names, _ = store.List()
	if len(names) != 0 {
		t.Fatalf("post-delete List() = %v, want empty", names)
	}

	cp, err := store.Load("deletable", "")
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Fatal("Load after Delete should return nil")
	}
}

func TestFileCheckpointStore_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got %v", err)
	}
}

func TestFileCheckpointStore_ImplementsCheckpointManager(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var _ CheckpointManager = store
}

func TestCheckpoint_RunID_Isolation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Save(Checkpoint{
		GraphName: "flow",
		RunID:     "run-1",
		NodeID:    "a",
		Board:     graph.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Save(Checkpoint{
		GraphName: "flow",
		RunID:     "run-2",
		NodeID:    "b",
		Board:     graph.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cp1, err := store.Load("flow", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if cp1 == nil || cp1.NodeID != "a" {
		t.Fatalf("expected run-1 checkpoint at node a, got %+v", cp1)
	}

	cp2, err := store.Load("flow", "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if cp2 == nil || cp2.NodeID != "b" {
		t.Fatalf("expected run-2 checkpoint at node b, got %+v", cp2)
	}
}

func TestCheckpoint_RunID_EmptyFallback(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Save(Checkpoint{
		GraphName: "flow",
		NodeID:    "x",
		Board:     graph.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cp, err := store.Load("flow", "")
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.NodeID != "x" {
		t.Fatalf("expected checkpoint at node x, got %+v", cp)
	}
}

func TestCheckpoint_RunID_LoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	cp, err := store.Load("flow", "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Fatal("expected nil for nonexistent run")
	}
}

func TestCheckpoint_RunID_InStruct(t *testing.T) {
	cp := Checkpoint{
		GraphName: "g",
		RunID:     "r-123",
		NodeID:    "n",
		Iteration: 5,
		Timestamp: time.Now(),
	}
	if cp.RunID != "r-123" {
		t.Fatalf("expected RunID=r-123, got %q", cp.RunID)
	}
}

func TestCheckpoint_WithRunID_IntegrationExecution(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileCheckpointStore(FileCheckpointConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	g := buildGraph("test", "a",
		map[string]graph.Node{
			"a": newTestNode("a", func(_ graph.ExecutionContext, b *graph.Board) error {
				b.SetVar("done", true)
				return nil
			}),
		},
		[]graph.Edge{{From: "a", To: graph.END}},
	)

	board := graph.NewBoard()
	exec := NewLocalExecutor()
	_, err = exec.Execute(context.Background(), g, board,
		WithRunID("run-abc"),
		WithCheckpointStore(store),
	)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	cp, err := store.Load("test", "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil {
		t.Fatal("expected checkpoint with runID")
	}
	if cp.RunID != "run-abc" {
		t.Fatalf("expected RunID=run-abc, got %q", cp.RunID)
	}
}
