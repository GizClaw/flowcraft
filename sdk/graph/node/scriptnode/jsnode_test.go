package scriptnode

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
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

func TestScriptNode_RuntimeLateBindingOverridesExtraAndChildInheritsParentBindings(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	src := `
		var sig = runtime.execScript(
			'if (extra.value() !== "from-extra") throw new Error("extra missing: " + extra.value());' +
			'board.setVar("child_extra", extra.value());' +
			'board.setVar("child_board", "ok");',
			null
		);
		if (sig !== null) throw new Error("expected null signal, got " + JSON.stringify(sig));
		board.setVar("parent_runtime", "ok");
	`
	fakeRuntimeBridge := bindings.BindingFunc(func(context.Context) (string, any) {
		return "runtime", map[string]any{"execScript": "not-callable"}
	})
	n := New("runtime_late", "script", src, nil, rt, testValueBridge("from-extra"), fakeRuntimeBridge)

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("parent_runtime"); got != "ok" {
		t.Fatalf("parent_runtime = %v, want ok", got)
	}
	if got, _ := board.GetVar("child_board"); got != "ok" {
		t.Fatalf("child_board = %v, want ok", got)
	}
	if got, _ := board.GetVar("child_extra"); got != "from-extra" {
		t.Fatalf("child_extra = %v, want from-extra", got)
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

func TestScriptNode_ParallelCancelNodeBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `
		var ok = parallel.cancelNode("draft_answer", "intent rejected");
		board.setVar("cancel_ok", ok);
	`
	n := New("decide", "script", src, nil, rt)
	controller := &stubParallelController{}

	board := graph.NewBoard()
	ctx := graph.ExecutionContext{
		Context: graph.WithParallelController(context.Background(), controller),
	}
	if err := n.ExecuteBoard(ctx, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	if v, _ := board.GetVar("cancel_ok"); v != true {
		t.Fatalf("cancel_ok = %v", v)
	}
	if controller.nodeID != "draft_answer" {
		t.Fatalf("nodeID = %q", controller.nodeID)
	}
	if controller.reason != "intent rejected" {
		t.Fatalf("reason = %q", controller.reason)
	}
}

func TestScriptNode_ParallelCancelNodeBridge_NoParallelContext(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `board.setVar("cancel_ok", parallel.cancelNode("draft_answer", "intent rejected"));`
	n := New("decide", "script", src, nil, rt)

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if v, _ := board.GetVar("cancel_ok"); v != false {
		t.Fatalf("cancel_ok = %v, want false", v)
	}
}

type stubParallelController struct {
	nodeID string
	reason string
}

func (s *stubParallelController) CancelNode(nodeID, reason string) bool {
	s.nodeID = nodeID
	s.reason = reason
	return true
}
