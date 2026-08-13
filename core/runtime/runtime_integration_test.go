package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

type integrationEngineFactory struct {
	identities chan agent.Identity
	delivered  <-chan struct{}
	buses      chan event.Bus
}

func (f *integrationEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "runtime-integration"}
}

func (f *integrationEngineFactory) New(context.Context, resource.Input) (any, error) {
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		bus, ok := agent.EventBusFromHost(host)
		if !ok || bus == nil {
			return board, errors.New("runtime host does not expose EventBus")
		}
		f.identities <- run.Identity
		f.buses <- bus

		if err := publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run); err != nil {
			return board, err
		}
		if err := agent.EmitStreamPart(ctx, host, run.RunID, run.AgentID+".integration",
			message.TextPart{Text: "hello"}); err != nil {
			return board, err
		}
		for range 2 {
			select {
			case <-f.delivered:
			case <-ctx.Done():
				return board, ctx.Err()
			}
		}

		board.AppendChannelMessage(
			agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "hello"),
		)
		if err := publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run); err != nil {
			return board, err
		}
		return board, nil
	}), nil
}

func publishRunEvent(
	ctx context.Context,
	host agent.Host,
	subject event.Subject,
	run agent.Run,
) error {
	envelope, err := event.NewEnvelope(ctx, subject, nil)
	if err != nil {
		return err
	}
	envelope.SetRunID(run.RunID)
	envelope.SetAgentID(run.AgentID)
	return host.Publish(ctx, envelope)
}

func TestRuntimePublicAPIEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	delivered := make(chan struct{}, 2)
	identities := make(chan agent.Identity, 1)
	buses := make(chan event.Bus, 1)
	reg := resource.NewRegistry()
	reg.MustRegister(&integrationEngineFactory{
		identities: identities,
		delivered:  delivered,
		buses:      buses,
	})
	reg.MustRegister(event.NewFactory())

	doc := parseRuntimeDoc(t, `version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
agents:
  echo:
    card: {name: Echo}
    engine:
      kind: runtime-integration
runtime:
  event_bus: events
  sessions:
    idle_timeout: 1m
    sink_buffer: 8
`)

	app, err := NewBuilder(reg).Build(ctx, doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID:   "echo",
		ContextID: "session-context",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = lease.Close() }()

	received := [2]chan agent.StreamDeltaPayload{
		make(chan agent.StreamDeltaPayload, 1),
		make(chan agent.StreamDeltaPayload, 1),
	}
	sinks := make([]session.SinkSpec, len(received))
	for index := range received {
		index := index
		sinks[index] = session.SinkSpec{
			ID: fmt.Sprintf("sink-%d", index),
			Sink: agent.StreamSinkFunc(func(
				_ context.Context,
				envelope event.Envelope,
				delta agent.StreamDeltaPayload,
			) error {
				if agent.IsStreamDelta(envelope.Subject) {
					received[index] <- delta
					delivered <- struct{}{}
				}
				return nil
			}),
		}
	}

	turn, err := lease.Session().Start(ctx, agent.Request{
		ContextID: "caller-context-must-be-replaced",
		RunID:     "caller-run-must-be-replaced",
		Message:   message.NewTextMessage(message.RoleUser, "hello"),
	}, sinks...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.StatusCompleted || result.RunID != turn.RunID() {
		t.Fatalf("result = %+v, turn RunID = %q", result, turn.RunID())
	}
	if turn.RunID() == "caller-run-must-be-replaced" {
		t.Fatal("Session did not replace the caller-supplied RunID")
	}

	identity := <-identities
	if identity.AgentID != "echo" || identity.ConversationID != "session-context" {
		t.Fatalf("engine identity = %+v", identity)
	}
	if identity.RunID != turn.RunID() {
		t.Fatalf("engine RunID = %q, turn RunID = %q", identity.RunID, turn.RunID())
	}
	if bus := <-buses; bus == nil {
		t.Fatal("engine received a nil EventBus from the runtime host")
	}
	for index, events := range received {
		select {
		case delta := <-events:
			if delta.Type != agent.StreamDeltaPart {
				t.Fatalf("sink %d delta type = %q", index, delta.Type)
			}
			if text, ok := delta.Part.(message.TextPart); !ok || text.Text != "hello" {
				t.Fatalf("sink %d delta part = %#v", index, delta.Part)
			}
		case <-ctx.Done():
			t.Fatalf("sink %d did not receive the stream event: %v", index, ctx.Err())
		}
	}

	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "echo", ContextID: "after-close",
	}); !errors.Is(err, session.ErrManagerClosed) {
		t.Fatalf("Open after Runtime.Close error = %v, want ErrManagerClosed", err)
	}
}
