package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// RunBridgeOptions configures NewRunBridge (workflow / agent metadata on the board).
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
