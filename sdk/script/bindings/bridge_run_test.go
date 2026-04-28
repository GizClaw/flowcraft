package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

func TestRunInfoBridge_AllFieldsExposed(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	info := agent.RunInfo{
		AgentID:   "agent-7",
		RunID:     "run-42",
		TaskID:    "task-9",
		ContextID: "ctx-1",
	}

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewRunInfoBridge(info))
	_, err := rt.Exec(context.Background(), "run", `
		if (run.get_run_id()     !== "run-42")  throw new Error("run_id");
		if (run.get_task_id()    !== "task-9")  throw new Error("task_id");
		if (run.get_agent_id()   !== "agent-7") throw new Error("agent_id");
		if (run.get_context_id() !== "ctx-1")   throw new Error("context_id");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunInfoBridge_EmptyRunInfo_ReturnsEmptyStrings(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))

	// All fields zero-valued — getters must return "" (not throw, not "undefined").
	env := bindings.BuildEnv(context.Background(), nil, bindings.NewRunInfoBridge(agent.RunInfo{}))
	_, err := rt.Exec(context.Background(), "run-empty", `
		if (run.get_run_id()     !== "") throw new Error("run_id should be empty");
		if (run.get_task_id()    !== "") throw new Error("task_id should be empty");
		if (run.get_agent_id()   !== "") throw new Error("agent_id should be empty");
		if (run.get_context_id() !== "") throw new Error("context_id should be empty");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunInfoBridge_PartialRunInfo_FalsyOnAbsent(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))

	// Only RunID set — confirms the documented `if (!run.get_task_id())`
	// pattern works for absence detection without a separate has_* probe.
	env := bindings.BuildEnv(context.Background(), nil, bindings.NewRunInfoBridge(agent.RunInfo{
		RunID: "run-only",
	}))
	_, err := rt.Exec(context.Background(), "run-partial", `
		if (run.get_run_id() !== "run-only") throw new Error("run_id");
		if (run.get_task_id())    throw new Error("task_id should be falsy");
		if (run.get_agent_id())   throw new Error("agent_id should be falsy");
		if (run.get_context_id()) throw new Error("context_id should be falsy");
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunInfoBridge_GetterCallsAreReadOnly(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	info := agent.RunInfo{RunID: "stable", TaskID: "t1"}

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewRunInfoBridge(info))
	_, err := rt.Exec(context.Background(), "run-stable", `
		// Re-reading must yield the same value — confirms the closure
		// captured the value, not a moving reference.
		var a = run.get_run_id();
		var b = run.get_run_id();
		if (a !== b || a !== "stable") throw new Error("run_id not stable: " + a + " vs " + b);
	`, env)
	if err != nil {
		t.Fatal(err)
	}
}
