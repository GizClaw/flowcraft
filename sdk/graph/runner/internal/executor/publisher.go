package executor

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// newNodePublisher builds the StreamPublisher handed to a node. The
// node speaks the simplified (eventType, payload) shape; this wrapper
// translates each emit into a fully-formed engine event.Envelope and
// pushes it through the executor's host publisher.
//
// The wrapper is always non-nil so nodes can call ctx.Publisher.Emit
// without nil-checks.
func newNodePublisher(ctx context.Context, cfg runConfig, nodeID string) graph.StreamPublisher {
	actorKey := actorKeyFrom(ctx)
	graphName := cfg.graphName
	pub := cfg.publisher

	return graph.StreamPublisherFunc(func(eventType string, payload any) {
		if pub == nil {
			return
		}
		pl := normalisePayload(eventType, payload)
		publishNodeEvent(ctx, pub, engine.SubjectStreamDelta(cfg.runID, nodeID),
			cfg.runID, graphName, actorKey, nodeID, pl)
	})
}

// normalisePayload guarantees a map shape with a "type" field so
// subject-only subscribers can still discriminate without inspecting
// the Subject suffix.
func normalisePayload(eventType string, payload any) map[string]any {
	out := map[string]any{"type": eventType}
	if m, ok := payload.(map[string]any); ok {
		for k, v := range m {
			if k == "type" {
				continue
			}
			out[k] = v
		}
	} else if payload != nil {
		out["payload"] = payload
	}
	return out
}
