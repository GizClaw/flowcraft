package bindings

import (
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// AgentStepOptions selects common bindings for one workflow agent step (Lua/JS),
// without pulling in LLM streaming (add NewStreamBridge at the call site if needed).
type AgentStepOptions struct {
	Board *workflow.Board
	// TaskID / RunID mirror workflow.Request fields; RunID overrides board when non-empty.
	TaskID string
	RunID  string

	ToolRegistry *tool.Registry
	// AllowedTools is passed to WithAllowedToolNames when non-empty (ignored if ToolRegistry is nil).
	AllowedTools []string
}

// AgentStepBindings returns a typical binding set: board, run, expr, optional tools.
// Caller still supplies config + script.Runtime-specific globals (signal, etc.).
func AgentStepBindings(o AgentStepOptions) []BindingFunc {
	var fns []BindingFunc
	if o.Board != nil {
		fns = append(fns, NewBoardBridge(o.Board))
	}
	fns = append(fns, NewRunBridge(RunBridgeOptions{
		Board:  o.Board,
		TaskID: o.TaskID,
		RunID:  o.RunID,
	}))
	fns = append(fns, NewExprBridge())
	if o.ToolRegistry != nil && len(o.AllowedTools) > 0 {
		fns = append(fns, NewToolBridge(o.ToolRegistry, WithAllowedToolNames(o.AllowedTools...)))
	}
	return fns
}
