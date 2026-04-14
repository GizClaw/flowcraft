package scriptnode

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

func TestScriptNode_IDAndType(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	n := New("n1", "router", "var x = 1;", nil, rt)
	if n.ID() != "n1" {
		t.Fatalf("ID() = %q", n.ID())
	}
	if n.Type() != "router" {
		t.Fatalf("Type() = %q", n.Type())
	}
}

func TestScriptNode_PortResolution(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	tests := []struct {
		nodeType   string
		wantInput  int
		wantOutput int
	}{
		{"router", 1, 1},
		{"ifelse", 1, 1},
		{"template", 1, 1},
		{"answer", 2, 1},
		{"assigner", 1, 0},
		{"loopguard", 0, 2},
		{"aggregator", 1, 1},
		{"gate", 1, 2},
		{"context", 2, 0},
		{"approval", 1, 1},
		{"iteration", 2, 1},
		{"unknown_type", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.nodeType, func(t *testing.T) {
			n := New("test", tt.nodeType, "", nil, rt)
			if len(n.InputPorts()) != tt.wantInput {
				t.Fatalf("InputPorts len = %d, want %d", len(n.InputPorts()), tt.wantInput)
			}
			if len(n.OutputPorts()) != tt.wantOutput {
				t.Fatalf("OutputPorts len = %d, want %d", len(n.OutputPorts()), tt.wantOutput)
			}
		})
	}
}

func TestScriptNode_ConfigGetSet(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	n := New("n1", "template", "", map[string]any{"key": "val"}, rt)
	if n.Config()["key"] != "val" {
		t.Fatal("Config() should return initial config")
	}
	n.SetConfig(map[string]any{"key": "updated"})
	if n.Config()["key"] != "updated" {
		t.Fatal("SetConfig() should update config")
	}
}

func TestScriptNode_ExecuteBoard(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `board.setVar("result", "executed");`
	n := New("n1", "template", src, nil, rt)

	board := graph.NewBoard()
	ctx := graph.ExecutionContext{
		Context: context.Background(),
		RunID:   "run1",
	}
	err := n.ExecuteBoard(ctx, board)
	if err != nil {
		t.Fatalf("ExecuteBoard error: %v", err)
	}
	v, ok := board.GetVar("result")
	if !ok || v != "executed" {
		t.Fatalf("result = %v, ok = %v", v, ok)
	}
}
