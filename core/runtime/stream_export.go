package runtime

import (
	"context"
	"maps"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// Stream target kinds understood by [StreamExportRegistry]. The
// registry only ever resolves targets whose kind appears here; unknown
// kinds from persisted backend records are rejected outright.
const (
	// StreamTargetConversation routes envelopes to a live,
	// conversation-scoped sink registered by the UI/runtime (the
	// destination survives the individual caller turn).
	StreamTargetConversation = "conversation"
	// StreamTargetBus forwards envelopes onto a runtime-owned event
	// bus under their original subject, so any subscriber (SSE
	// relay, dashboard) can consume async subagent streams.
	StreamTargetBus = "bus"
)

// StreamExportRegistry is the runtime-owned bridge between the
// serializable stream targets persisted in async delegation records
// and the runtime's live stream transports.
//
// It implements both halves of the delegation stream contract:
//   - Exporter describes sinks the runtime attached to a turn as
//     serializable targets at async submit time;
//   - Resolver re-materializes those targets worker-side when no
//     in-process escrow entry survives (cross-process backends).
//
// Resolver is whitelisted by construction: it only accepts the kinds
// declared above, and conversation sinks come from the registry's own
// registered set — never from request data.
type StreamExportRegistry struct {
	mu            sync.Mutex
	conversations map[string]agent.StreamSink
	buses         map[string]event.Bus
}

// NewStreamExportRegistry builds a registry over the given whitelist
// of named event buses. conversations is empty until the UI attaches
// through RegisterConversation.
func NewStreamExportRegistry(buses map[string]event.Bus) *StreamExportRegistry {
	return &StreamExportRegistry{
		conversations: make(map[string]agent.StreamSink),
		buses:         maps.Clone(buses),
	}
}

// RegisterConversation attaches a live, conversation-scoped sink that
// outlives individual turns. UI layers call this when they open a
// conversation and UnregisterConversation when they detach. Nil sinks
// are ignored.
func (r *StreamExportRegistry) RegisterConversation(contextID string, sink agent.StreamSink) {
	if contextID == "" || isNil(sink) {
		return
	}
	wrapped := conversationStreamSink{contextID: contextID, inner: sink}
	r.mu.Lock()
	r.conversations[contextID] = wrapped
	r.mu.Unlock()
}

// UnregisterConversation removes the conversation's sink. Idempotent.
func (r *StreamExportRegistry) UnregisterConversation(contextID string) {
	if contextID == "" {
		return
	}
	r.mu.Lock()
	delete(r.conversations, contextID)
	r.mu.Unlock()
}

// ConversationSink returns the registered live sink for a conversation,
// wrapped with its context id so [StreamExportRegistry.Exporter] can
// describe it. Turn attachment should use this instance as the
// SinkSpec.Sink so the async exporter recognizes the destination.
// ok=false when the conversation is not attached.
func (r *StreamExportRegistry) ConversationSink(contextID string) (agent.StreamSink, bool) {
	if contextID == "" {
		return nil, false
	}
	r.mu.Lock()
	sink, ok := r.conversations[contextID]
	r.mu.Unlock()
	if !ok || isNil(sink) {
		return nil, false
	}
	return sink, true
}

// Exporter implements delegation.StreamTargetExporter: it recognizes
// sinks registered through RegisterConversation (and only those) and
// describes them as conversation targets. All other sinks report
// ok=false.
func (r *StreamExportRegistry) Exporter(spec session.SinkSpec) (delegation.StreamTarget, bool) {
	sink, ok := spec.Sink.(conversationStreamSink)
	if !ok || isNil(sink.inner) {
		return delegation.StreamTarget{}, false
	}
	return delegation.StreamTarget{
		Kind: StreamTargetConversation,
		ID:   sink.contextID,
	}, true
}

// Resolver implements delegation.StreamTargetResolver with a strict
// kind whitelist. conversation targets resolve to the registered live
// sink; bus targets resolve to a fixed forwarder onto the named bus.
// Unknown kinds are policy-denied; known kinds with unknown ids are
// not-found or validation errors. No sink is ever constructed from
// free-form persisted data.
func (r *StreamExportRegistry) Resolver(
	ctx context.Context,
	target delegation.StreamTarget,
) (agent.StreamSink, error) {
	switch target.Kind {
	case StreamTargetConversation:
		if target.ID == "" {
			return nil, errdefs.Validationf(
				"runtime stream export: conversation target id is required")
		}
		r.mu.Lock()
		sink, ok := r.conversations[target.ID]
		r.mu.Unlock()
		if !ok || isNil(sink) {
			return nil, errdefs.NotAvailablef(
				"runtime stream export: conversation %q has no attached sink",
				target.ID)
		}
		return sink, nil
	case StreamTargetBus:
		if target.ID == "" {
			return nil, errdefs.Validationf(
				"runtime stream export: bus target id is required")
		}
		r.mu.Lock()
		bus, ok := r.buses[target.ID]
		r.mu.Unlock()
		if !ok || isNil(bus) {
			return nil, errdefs.Validationf(
				"runtime stream export: unknown bus %q", target.ID)
		}
		return busStreamSink{busName: target.ID, bus: bus}, nil
	default:
		return nil, errdefs.PolicyDeniedf(
			"runtime stream export: target kind %q is not whitelisted",
			target.Kind)
	}
}

// conversationStreamSink wraps a conversation-scoped sink with its
// context id so [StreamExportRegistry.Exporter] can recognize it.
// OnDelta forwards to the wrapped sink unchanged.
type conversationStreamSink struct {
	contextID string
	inner     agent.StreamSink
}

func (s conversationStreamSink) OnDelta(
	ctx context.Context,
	env event.Envelope,
	delta agent.StreamDeltaPayload,
) error {
	return s.inner.OnDelta(ctx, env, delta)
}

// busStreamSink forwards every stream envelope onto a named bus under
// its original subject, so subscribers can pick up async subagent
// streams without a live per-turn sink.
type busStreamSink struct {
	busName string
	bus     event.Bus
}

func (s busStreamSink) OnDelta(
	ctx context.Context,
	env event.Envelope,
	_ agent.StreamDeltaPayload,
) error {
	return s.bus.Publish(ctx, env)
}

var (
	_ agent.StreamSink = conversationStreamSink{}
	_ agent.StreamSink = busStreamSink{}
)
