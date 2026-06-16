package scriptnode

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/script"
)

// newParallelBridge exposes graph parallel-fork controls to scripts.
//
// Script-facing API:
//
//	parallel.cancelNode(nodeID, reason) bool
func newParallelBridge() script.BindingFunc {
	return func(ctx context.Context) (string, any) {
		return "parallel", map[string]any{
			"cancelNode": func(nodeID, reason string) bool {
				controller, ok := graph.ParallelControllerFromContext(ctx)
				if !ok {
					return false
				}
				return controller.CancelNode(nodeID, reason)
			},
		}
	}
}
