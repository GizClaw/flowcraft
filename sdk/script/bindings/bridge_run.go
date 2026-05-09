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
// Naming: the constructor advertises its data source (RunInfo) rather
// than using the bare "Run" prefix. The previous workflow-coupled
// NewRunBridge was retired in v0.3.0; this is now the only run-metadata
// bridge in the package.
//
// Design choices:
//
//   - Pulls metadata from agent.RunInfo directly. No board lookup, no
//     VarRunID convention key. The information lives on the call stack
//     where it was minted, not in a side-channel string map.
//
//   - No board reference. Run identity and board state are independent
//     concerns; the bridge has no reason to know about the board.
//
//   - Exposes AgentID and ContextID in addition to RunID/TaskID — scripts
//     that route on multi-agent or multi-conversation identity need them.
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
