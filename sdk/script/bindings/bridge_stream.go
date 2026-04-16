package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// NewStreamBridge exposes streaming as global "stream" (emit).
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
