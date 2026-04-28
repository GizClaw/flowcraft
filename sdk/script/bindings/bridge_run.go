package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

// NewRunInfoBridge exposes read-only run metadata to scripts as global "run",
// sourcing every field from a single agent.RunInfo value.
//
// Script-facing API:
//
//	run.get_run_id()      string  // agent.RunInfo.RunID
//	run.get_task_id()     string  // agent.RunInfo.TaskID
//	run.get_agent_id()    string  // agent.RunInfo.AgentID
//	run.get_context_id()  string  // agent.RunInfo.ContextID
//
// All getters return the empty string when the corresponding field on
// agent.RunInfo is unset; scripts can do `if (!run.get_task_id()) { … }`
// to branch on absence without needing a separate "has_*" probe.
//
// Naming: the legacy NewRunBridge in deprecated.go is kept under its
// original name to avoid breaking existing callers; the new constructor
// names its data source (RunInfo) instead. Once the legacy bridge is
// removed in v0.3.0, this will be renamed to NewRunBridge.
//
// Design choices vs. the deprecated NewRunBridge in deprecated.go:
//
//   - Pulls metadata from agent.RunInfo directly. No board lookup, no
//     workflow.VarRunID convention key. The information lives on the call
//     stack where it was minted, not in a side-channel string map.
//
//   - No board reference. The previous bridge accepted a *workflow.Board
//     so it could fall back to reading "__run_id" — pure tech debt from
//     when run state was scattered across the blackboard. The agent runtime
//     hands you the full RunInfo, so the bridge has no reason to know
//     about a board at all.
//
//   - Exposes AgentID and ContextID in addition to RunID/TaskID. These
//     two fields exist on agent.RunInfo and were not surfaced by the old
//     bridge; scripts that route on multi-agent or multi-conversation
//     identity need them.
//
//   - Takes the value (not a pointer). RunInfo is a small immutable
//     descriptor; copying it into the closure is cheaper and safer than
//     carrying a pointer into script-controlled territory.
//
// Typical wiring at the agent.Run boundary:
//
//	env := bindings.BuildEnv(ctx, scriptCfg,
//	    bindings.NewBoardBridge(eng.Board()),
//	    bindings.NewRunInfoBridge(runInfo),
//	    bindings.NewExprBridge(),
//	)
func NewRunInfoBridge(info agent.RunInfo) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "run", map[string]any{
			"get_run_id":     func() string { return info.RunID },
			"get_task_id":    func() string { return info.TaskID },
			"get_agent_id":   func() string { return info.AgentID },
			"get_context_id": func() string { return info.ContextID },
		}
	}
}
