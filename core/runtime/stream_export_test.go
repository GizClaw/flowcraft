package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

func TestStreamExportRegistry_ConversationLifecycle(t *testing.T) {
	reg := NewStreamExportRegistry(nil)
	received := make(chan agent.StreamDeltaPayload, 4)
	inner := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})

	reg.RegisterConversation("ctx-1", inner)
	sink, ok := reg.ConversationSink("ctx-1")
	if !ok {
		t.Fatal("ConversationSink missing after register")
	}
	if _, ok := sink.(conversationStreamSink); !ok {
		t.Fatalf("registered sink type = %T, want conversationStreamSink", sink)
	}

	// Exporter describes the registry's own sinks.
	target, ok := reg.Exporter(session.SinkSpec{ID: "conv", Sink: sink})
	if !ok || target.Kind != StreamTargetConversation || target.ID != "ctx-1" {
		t.Fatalf("Exporter = %+v, %v; want conversation target ctx-1", target, ok)
	}
	// Exporter ignores foreign sinks.
	if _, ok := reg.Exporter(session.SinkSpec{
		ID: "foreign", Sink: agent.StreamSinkFunc(func(
			context.Context, event.Envelope, agent.StreamDeltaPayload,
		) error {
			return nil
		}),
	}); ok {
		t.Fatal("Exporter described a foreign sink")
	}

	// Resolver returns the registered sink and it forwards deltas.
	resolved, err := reg.Resolver(context.Background(), target)
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	if err := resolved.OnDelta(context.Background(), event.Envelope{},
		agent.StreamDeltaPayload{Type: agent.StreamDeltaPart,
			Part: message.TextPart{Text: "x"}}); err != nil {
		t.Fatalf("OnDelta: %v", err)
	}
	select {
	case delta := <-received:
		if delta.Type != agent.StreamDeltaPart {
			t.Fatalf("delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("resolved sink did not forward delta to inner sink")
	}

	reg.UnregisterConversation("ctx-1")
	if _, ok := reg.ConversationSink("ctx-1"); ok {
		t.Fatal("ConversationSink still present after unregister")
	}
	if _, err := reg.Resolver(context.Background(), target); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resolver after unregister error = %v, want not available", err)
	}
	reg.UnregisterConversation("ctx-1")    // idempotent
	reg.RegisterConversation("ctx-1", nil) // ignored
	if _, ok := reg.ConversationSink("ctx-1"); ok {
		t.Fatal("nil register attached a sink")
	}
}

func TestStreamExportRegistry_ResolverWhitelist(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	reg := NewStreamExportRegistry(map[string]event.Bus{"events": bus})

	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: "unknown", ID: "x",
	}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("unknown kind error = %v, want policy denied", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetConversation,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("empty conversation id error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetConversation, ID: "nope",
	}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("unknown conversation error = %v, want not available", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetBus,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("empty bus id error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetBus, ID: "missing-bus",
	}); !errdefs.IsValidation(err) {
		t.Fatalf("unknown bus error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetBus, ID: "events",
	}); err != nil {
		t.Fatalf("known bus error = %v", err)
	}
}

func TestStreamExportRegistry_BusForwardsEnvelopes(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	reg := NewStreamExportRegistry(map[string]event.Bus{"events": bus})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := bus.Subscribe(ctx, event.Pattern("nrun.>"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sink, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: StreamTargetBus, ID: "events",
	})
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	env := event.Envelope{Subject: "nrun.run-1.agent-x.node.work"}
	if err := sink.OnDelta(context.Background(), env, agent.StreamDeltaPayload{}); err != nil {
		t.Fatalf("OnDelta: %v", err)
	}
	select {
	case got := <-sub.C():
		if got.Subject != env.Subject {
			t.Fatalf("forwarded subject = %q, want %q", got.Subject, env.Subject)
		}
	case <-time.After(time.Second):
		t.Fatal("bus sink did not forward envelope to subscribers")
	}
}
