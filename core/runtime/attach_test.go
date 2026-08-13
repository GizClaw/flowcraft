package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

const attachEngineKind = "runtime-attach-engine"

type attachEngineFactory struct {
	engine agent.Engine
}

func (f attachEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: attachEngineKind}
}

func (f attachEngineFactory) New(context.Context, resource.Input) (any, error) {
	if f.engine == nil {
		return nil, errors.New("attach engine factory: nil engine")
	}
	return f.engine, nil
}

func TestRuntimeAttachDeliversPromptLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		reply, err := host.AskUser(ctx, agent.UserPrompt{Source: "attach-test"})
		if err != nil {
			return board, err
		}
		if len(reply.Parts) == 0 {
			return board, errors.New("AskUser reply missing parts")
		}
		return board, nil
	})
	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(attachEngineFactory{engine: engine})

	doc := parseRuntimeDoc(t, `version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
agents:
  echo:
    card: {name: Echo}
    engine:
      kind: runtime-attach-engine
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

	requested := make(chan session.PromptRequested, 1)
	resolved := make(chan session.PromptResolved, 1)
	requestedDetach, err := app.Attach(ctx, session.PatternPromptRequested(),
		event.SinkFunc(func(ctx context.Context, env event.Envelope) error {
			var req session.PromptRequested
			if err := env.Decode(&req); err != nil {
				return err
			}
			requested <- req
			return nil
		}))
	if err != nil {
		t.Fatalf("Attach requested: %v", err)
	}
	defer requestedDetach()
	resolvedDetach, err := app.Attach(ctx, session.PatternPromptResolved(),
		event.SinkFunc(func(ctx context.Context, env event.Envelope) error {
			var res session.PromptResolved
			if err := env.Decode(&res); err != nil {
				return err
			}
			resolved <- res
			return nil
		}))
	if err != nil {
		t.Fatalf("Attach resolved: %v", err)
	}
	defer resolvedDetach()

	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID:   "echo",
		ContextID: "attach-test",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(ctx, agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var req session.PromptRequested
	select {
	case req = <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PromptRequested")
	}
	if req.PromptID == "" || req.Prompt.Source != "attach-test" {
		t.Fatalf("PromptRequested = %+v, want source attach-test", req)
	}

	if err := turn.Reply(ctx, req.PromptID, agent.UserReply{
		Parts: []message.Part{message.TextPart{Text: "ok"}},
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	select {
	case res := <-resolved:
		if res.PromptID != req.PromptID || res.Status != session.PromptReplied {
			t.Fatalf("PromptResolved = %+v, want prompt %s replied", res, req.PromptID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PromptResolved")
	}

	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestRuntimeAttachFailsAfterClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(attachEngineFactory{engine: agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})})

	doc := parseRuntimeDoc(t, `version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
agents:
  echo:
    card: {name: Echo}
    engine:
      kind: runtime-attach-engine
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
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := app.Attach(ctx, session.PatternPromptRequested(),
		event.SinkFunc(func(context.Context, event.Envelope) error { return nil })); err == nil {
		t.Fatal("Attach after Close succeeded, want NotAvailable")
	}
}
