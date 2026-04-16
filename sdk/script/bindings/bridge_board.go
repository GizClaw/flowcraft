package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// NewBoardBridge exposes workflow board variables as global "board".
func NewBoardBridge(board *workflow.Board) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "board", map[string]any{
			"getVar":  func(key string) any { v, _ := board.GetVar(key); return v },
			"setVar":  func(key string, value any) { board.SetVar(key, value) },
			"getVars": func() map[string]any { return board.Vars() },
			"hasVar":  func(key string) bool { _, ok := board.GetVar(key); return ok },
		}
	}
}
