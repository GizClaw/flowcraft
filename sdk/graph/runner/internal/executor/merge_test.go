package executor

import (
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestMergeLastWins(t *testing.T) {
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"x": "a"}, channels: map[string][]model.Message{}},
		{vars: map[string]any{"x": "b", "y": "only_b"}, channels: map[string][]model.Message{}},
	}

	if err := mergeLastWins(board, snap, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := board.GetVarString("x"); v != "b" {
		t.Fatalf("expected last-wins x=b, got %q", v)
	}
	if v := board.GetVarString("y"); v != "only_b" {
		t.Fatalf("expected y=only_b, got %q", v)
	}
}

func TestMergeNamespace(t *testing.T) {
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"out": "from_0"}, channels: map[string][]model.Message{}},
		{vars: map[string]any{"out": "from_1"}, channels: map[string][]model.Message{}},
	}

	if err := mergeNamespace(board, snap, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := board.GetVarString("__branch_0.out"); v != "from_0" {
		t.Fatalf("expected __branch_0.out=from_0, got %q", v)
	}
	if v := board.GetVarString("__branch_1.out"); v != "from_1" {
		t.Fatalf("expected __branch_1.out=from_1, got %q", v)
	}
}

func TestMergeErrorOnConflict_NoConflict(t *testing.T) {
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"a": "1"}, channels: map[string][]model.Message{}},
		{vars: map[string]any{"b": "2"}, channels: map[string][]model.Message{}},
	}

	if err := mergeErrorOnConflict(board, snap, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if board.GetVarString("a") != "1" || board.GetVarString("b") != "2" {
		t.Fatal("vars not merged correctly")
	}
}

func TestMergeErrorOnConflict_Conflict(t *testing.T) {
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"shared": "from_a"}, channels: map[string][]model.Message{}},
		{vars: map[string]any{"shared": "from_b"}, channels: map[string][]model.Message{}},
	}

	err := mergeErrorOnConflict(board, snap, results)
	if err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestMergeErrorOnConflict_ChannelConflict(t *testing.T) {
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{
			vars:     map[string]any{},
			channels: map[string][]model.Message{"ch": {model.NewTextMessage(model.RoleUser, "a")}},
		},
		{
			vars:     map[string]any{},
			channels: map[string][]model.Message{"ch": {model.NewTextMessage(model.RoleUser, "b")}},
		},
	}

	err := mergeErrorOnConflict(board, snap, results)
	if err == nil {
		t.Fatal("expected channel conflict error")
	}
}

func TestRegisterMergeStrategy_Custom(t *testing.T) {
	customName := MergeStrategy("test_concat")
	RegisterMergeStrategy(customName, func(board *graph.Board, _ *graph.BoardSnapshot, results []branchResult) error {
		for i, r := range results {
			for k, v := range r.vars {
				board.SetVar(fmt.Sprintf("%s_%d", k, i), v)
			}
		}
		return nil
	})
	defer delete(mergeRegistry, customName)

	fn := lookupMerge(customName)
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"val": "x"}, channels: map[string][]model.Message{}},
		{vars: map[string]any{"val": "y"}, channels: map[string][]model.Message{}},
	}

	if err := fn(board, snap, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if board.GetVarString("val_0") != "x" {
		t.Fatalf("expected val_0=x, got %q", board.GetVarString("val_0"))
	}
	if board.GetVarString("val_1") != "y" {
		t.Fatalf("expected val_1=y, got %q", board.GetVarString("val_1"))
	}
}

func TestLookupMerge_UnknownFallsBackToLastWins(t *testing.T) {
	fn := lookupMerge("nonexistent_strategy")
	board := graph.NewBoard()
	snap := board.Snapshot()

	results := []branchResult{
		{vars: map[string]any{"k": "v"}, channels: map[string][]model.Message{}},
	}
	if err := fn(board, snap, results); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if board.GetVarString("k") != "v" {
		t.Fatal("fallback should use last_wins")
	}
}

func TestChannelMessagesEqual(t *testing.T) {
	a := []model.Message{model.NewTextMessage(model.RoleUser, "hi")}
	b := []model.Message{model.NewTextMessage(model.RoleUser, "hi")}
	c := []model.Message{model.NewTextMessage(model.RoleAssistant, "hello")}

	if !channelMessagesEqual(a, b) {
		t.Fatal("identical messages should be equal")
	}
	if channelMessagesEqual(a, c) {
		t.Fatal("different messages should not be equal")
	}
	if channelMessagesEqual(a, nil) {
		t.Fatal("different lengths should not be equal")
	}
}
