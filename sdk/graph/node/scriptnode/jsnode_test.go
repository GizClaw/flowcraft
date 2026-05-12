package scriptnode

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
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

// signal.interrupt(msg) must surface as an engine.Interrupted error
// carrying CauseCustom so the agent layer can both recognise the pause
// (errdefs.IsInterrupted) and read the script-supplied detail.
func TestScriptNode_SignalInterrupt_CarriesCauseAndDetail(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `signal.interrupt("need approval");`
	n := New("n_pause", "template", src, nil, rt)

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
	}, graph.NewBoard())

	if !errdefs.IsInterrupted(err) {
		t.Fatalf("expected errdefs.IsInterrupted, got: %v", err)
	}
	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("error did not wrap engine.InterruptedError: %v", err)
	}
	if ie.Cause != engine.CauseCustom {
		t.Fatalf("Cause = %q, want %q", ie.Cause, engine.CauseCustom)
	}
	if ie.Detail != "need approval" {
		t.Fatalf("Detail = %q, want %q", ie.Detail, "need approval")
	}
}

// TestScriptNode_RunInfo_AllFieldsPropagate is the regression test
// for contract-audit #12. Before this fix, the script bridge was
// constructed with only RunID populated; agent_id() / task_id() /
// context_id() always returned "" in scripts even when agent.Run
// had written them upstream into engine.Run.Attributes.
//
// The fix wires ExecutionContext.Attributes (forwarded by the
// runner from engine.Run.Attributes) into agent.RunInfoFromAttributes,
// producing a fully populated RunInfo for the script binding.
func TestScriptNode_RunInfo_AllFieldsPropagate(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `
		board.setVar("agent_id", run.get_agent_id());
		board.setVar("task_id", run.get_task_id());
		board.setVar("context_id", run.get_context_id());
		board.setVar("run_id", run.get_run_id());
	`
	n := New("rinfo", "template", src, nil, rt)

	board := graph.NewBoard()
	ctx := graph.ExecutionContext{
		Context: context.Background(),
		RunID:   "run-x",
		Attributes: map[string]string{
			telemetry.AttrAgentID:        "researcher",
			telemetry.AttrTaskID:         "task-7",
			telemetry.AttrConversationID: "thread-3",
			// AttrRunID is intentionally absent: ExecutionContext.RunID
			// is the dedicated source agent.RunInfoFromAttributes
			// trusts over any attribute copy.
		},
	}
	if err := n.ExecuteBoard(ctx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	checks := map[string]string{
		"agent_id":   "researcher",
		"task_id":    "task-7",
		"context_id": "thread-3",
		"run_id":     "run-x",
	}
	for k, want := range checks {
		got, _ := board.GetVar(k)
		if got != want {
			t.Errorf("script-visible %s = %v, want %q", k, got, want)
		}
	}
}
