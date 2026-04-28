package executor

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// publisherFunc adapts a plain function into engine.Publisher so the
// executor can compose / fan out without leaking implementation types.
// It mirrors http.HandlerFunc / graph.StreamPublisherFunc and lives
// inside the executor package because engine intentionally keeps its
// public surface minimal.
type publisherFunc func(ctx context.Context, env event.Envelope) error

// Publish satisfies engine.Publisher.
func (f publisherFunc) Publish(ctx context.Context, env event.Envelope) error {
	if f == nil {
		return nil
	}
	return f(ctx, env)
}

// resolvePublisher collapses the executor's two possible event inputs
// (cfg.host — modern; cfg.bus — deprecated WithEventBus) into a single
// engine.Publisher consumed by the rest of the executor. The result is
// always non-nil so call sites never need to nil-check.
//
// Resolution rules:
//   - host alone: host is the publisher (Host implements Publisher).
//   - bus  alone: wrap bus so it satisfies Publisher.
//   - both: fan out to host AND bus, ignoring per-sink errors so a
//     clogged observer never aborts a run ("events are observability,
//     not control flow").
//   - neither: NoopHost{}.
//
// This helper is the single seam where the deprecated bus path enters
// the executor. After Execute calls it, cfg.bus is read by no other
// code; the rest of the executor only touches cfg.publisher.
func resolvePublisher(host engine.Host, bus event.Bus) engine.Publisher {
	hostNonNil := host != nil
	busNonNil := bus != nil && !isNoopBus(bus)
	switch {
	case !hostNonNil && !busNonNil:
		return engine.NoopHost{}
	case !busNonNil:
		return host
	case !hostNonNil:
		return publisherFunc(func(ctx context.Context, env event.Envelope) error {
			_ = bus.Publish(ctx, env)
			return nil
		})
	default:
		return publisherFunc(func(ctx context.Context, env event.Envelope) error {
			_ = host.Publish(ctx, env)
			_ = bus.Publish(ctx, env)
			return nil
		})
	}
}

// isNoopBus lets resolvePublisher skip the fan-out wrapper when the
// caller passed event.NoopBus{} explicitly (e.g. defaults set by
// runner.New). This avoids paying for an extra closure + Publish call
// per envelope on the common host-only path.
func isNoopBus(bus event.Bus) bool {
	_, ok := bus.(event.NoopBus)
	return ok
}

// newNodePublisher builds the StreamPublisher handed to a node. The
// node speaks the simplified (eventType, payload) shape; this wrapper
// translates each emit into a fully-formed engine event.Envelope and
// pushes it through the executor's composed publisher (host + optional
// legacy bus). The deprecated StreamCallback registered via
// WithStreamCallback is also fanned to here for v0.2 backwards
// compatibility.
//
// The wrapper is always non-nil so nodes can call ctx.Publisher.Emit
// without nil-checks.
func newNodePublisher(ctx context.Context, cfg runConfig, nodeID string) graph.StreamPublisher {
	actorKey := actorKeyFrom(ctx)
	graphName := cfg.graphName
	pub := cfg.publisher
	cb := cfg.streamCallback

	return graph.StreamPublisherFunc(func(eventType string, payload any) {
		if cb != nil {
			cb(graph.StreamEvent{Type: eventType, NodeID: nodeID, Payload: payload})
		}
		if pub == nil {
			return
		}
		pl := normalisePayload(eventType, payload)
		publishNodeEvent(ctx, pub, subjNodeStreamDelta(cfg.runID, nodeID),
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
