package script

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/graph"
)

// newParallelBridge exposes graph parallel-fork controls to scripts.
// The controller travels on the execution context: it exists only
// while a parallel wave is in flight, so cancelNode returns false
// (fail open) for scripts running outside one.
//
// Script-facing API:
//
//	parallel.cancelNode(nodeID, reason) bool
func newParallelBridge() bindings.BindingFunc {
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
