package bindings

// This file collects bridge entry points scheduled for removal in v0.3.0.
// They are kept here, isolated from the active surface, so the rest of the
// package can evolve free of workflow-era assumptions while existing callers
// keep compiling.
//
// Removal criteria for each symbol below:
//   - All in-tree consumers have migrated off the workflow streaming /
//     control-plane model.
//   - The replacement (typically engine.Board + agent runtime) is stable.
//
// Do not add new code here. Add new bridges as bridge_xxx.go.

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// NewStreamBridge exposes streaming as global "stream" (emit).
//
// Deprecated: scheduled for removal in v0.3.0. Tied to workflow.StreamCallback
// and currently only consumed by graph/node/scriptnode (which itself rides on
// workflow's streaming model). Will be replaced once graph migrates off
// workflow.StreamCallback.
func NewStreamBridge(stream workflow.StreamCallback, nodeID string) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "stream", map[string]any{
			"emit": func(eventType string, payload any) {
				if stream != nil {
					stream(workflow.StreamEvent{Type: eventType, NodeID: nodeID, Payload: payload})
				}
			},
		}
	}
}

// RunBridgeOptions configures NewRunBridge (workflow / agent metadata on the board).
//
// Deprecated: scheduled for removal in v0.3.0 together with NewRunBridge.
// See NewRunBridge for the engine/agent-era replacement plan.
type RunBridgeOptions struct {
	Board *workflow.Board
	// TaskID is optional (e.g. workflow.Request.TaskID).
	TaskID string
	// RunID, if non-empty, is returned by get_run_id instead of reading the board.
	RunID string
}

// NewRunBridge exposes read-only run metadata to scripts as global "run":
//   - get_run_id() string
//   - get_task_id() string
//
// Deprecated: scheduled for removal in v0.3.0. Hard-wired to *workflow.Board
// and the workflow.VarRunID convention. The engine/agent stack carries the
// same metadata as agent.RunInfo (RunID/TaskID/AgentID/ContextID) — pass it
// in directly without going through a board key. The replacement bridge will
// land alongside the rest of the agent-runtime cleanup.
func NewRunBridge(opts RunBridgeOptions) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "run", map[string]any{
			"get_run_id": func() string {
				if opts.RunID != "" {
					return opts.RunID
				}
				if opts.Board == nil {
					return ""
				}
				return opts.Board.GetVarString(workflow.VarRunID)
			},
			"get_task_id": func() string {
				return opts.TaskID
			},
		}
	}
}

// AgentStepOptions selects common bindings for one workflow agent step (Lua/JS),
// without pulling in LLM streaming (add NewStreamBridge at the call site if needed).
//
// Deprecated: scheduled for removal in v0.3.0 together with AgentStepBindings.
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
//
// Deprecated: scheduled for removal in v0.3.0. The agent/engine stack does not
// need this preset — callers compose BuildEnv with the bridges they want
// directly (typically four lines), and binding combinations vary too much per
// agent for one preset to be worth maintaining.
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
