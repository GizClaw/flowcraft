package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

func TestRunBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	b := workflow.NewBoard()
	b.SetVar(workflow.VarRunID, "run-xyz")

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewRunBridge(bindings.RunBridgeOptions{Board: b, TaskID: "task-1"}),
	)
	_, err := rt.Exec(context.Background(), "run", `
		if (run.get_run_id() !== "run-xyz") throw new Error("run id");
		if (run.get_task_id() !== "task-1") throw new Error("task id");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunBridge_OverrideRunID(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	b := workflow.NewBoard()
	b.SetVar(workflow.VarRunID, "from-board")

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewRunBridge(bindings.RunBridgeOptions{Board: b, RunID: "override"}),
	)
	_, err := rt.Exec(context.Background(), "run2", `
		if (run.get_run_id() !== "override") throw new Error("override");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_Allowed(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "echo", Description: "echo"},
		func(_ context.Context, args string) (string, error) {
			return "got:" + args, nil
		},
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg, bindings.WithAllowedToolNames("echo")),
	)
	_, err := rt.Exec(context.Background(), "tools", `
		var r = tools.call("echo", "{\"x\":1}");
		if (r.is_error) throw new Error(r.content);
		if (r.content.indexOf("got:") !== 0) throw new Error(r.content);
		var names = tools.list();
		if (names.length !== 1 || names[0] !== "echo") throw new Error("list");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolBridge_DenyByDefault(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "echo", Description: "echo"},
		func(_ context.Context, _ string) (string, error) { return "ok", nil },
	))

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewToolBridge(reg),
	)
	_, err := rt.Exec(context.Background(), "deny", `
		var r = tools.call("echo", "{}");
		if (!r.is_error) throw new Error("expected deny");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentStepBindings(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "ping", Description: "p"},
		func(_ context.Context, _ string) (string, error) { return "pong", nil },
	))
	b := workflow.NewBoard()
	b.SetVar(workflow.VarRunID, "r1")

	fns := bindings.AgentStepBindings(bindings.AgentStepOptions{
		Board:        b,
		TaskID:       "t1",
		ToolRegistry: reg,
		AllowedTools: []string{"ping"},
	})
	if len(fns) != 4 {
		t.Fatalf("len = %d, want 4 (board, run, expr, tools)", len(fns))
	}

	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil, fns...)
	_, err := rt.Exec(context.Background(), "step", `
		board.setVar("x", tools.call("ping", "{}").content);
	`, env)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := b.GetVar("x")
	if v != "pong" {
		t.Fatalf("x = %v", v)
	}
}
