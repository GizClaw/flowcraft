package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

type testHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h testHost) Publish(ctx context.Context, env event.Envelope) error {
	return h.bus.Publish(ctx, env)
}

func (h testHost) EventBus() event.Bus { return h.bus }

func withRunEnd(engine agent.Engine) agent.Engine {
	return agent.EngineFunc(func(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		result, err := engine.Execute(ctx, run, host, board)
		envelope, envelopeErr := event.NewEnvelope(context.WithoutCancel(ctx), agent.SubjectRunEnd(run.RunID), nil)
		if envelopeErr == nil {
			envelope.SetRunID(run.RunID)
			envelopeErr = host.Publish(context.WithoutCancel(ctx), envelope)
		}
		if err != nil {
			return result, err
		}
		return result, envelopeErr
	})
}

func newTurnSession(t *testing.T, engine agent.Engine, makeFactory func(event.Bus) HostFactory, options ...ManagerOption) (*Manager, *Session, *agent.StreamRouter, event.Bus) {
	t.Helper()
	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	instance := &deploy.Instance{Agent: agent.Agent{ID: "agent-a"}, Engine: withRunEnd(engine)}
	factory := makeFactory(bus)
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		factory,
		router,
		append([]ManagerOption{WithIdleTimeout(time.Minute), WithSinkBufferSize(8)}, options...)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lease.Close()
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	return manager, lease.Session(), router, bus
}

func TestSessionStartOverridesIdentityAndAttachesBeforeExecute(t *testing.T) {
	var got agent.Request
	var sinkCalls atomic.Int64
	firstEvent := make(chan struct{})
	var firstEventOnce sync.Once
	bus := event.NewMemoryBus()
	host := testHost{bus: bus}
	engine := agent.EngineFunc(func(ctx context.Context, run agent.Run, h agent.Host, board *agent.Board) (*agent.Board, error) {
		got.ContextID = run.ConversationID
		got.RunID = run.RunID
		env, err := event.NewEnvelope(ctx, agent.SubjectRunStart(run.RunID), nil)
		if err != nil {
			return board, err
		}
		env.SetRunID(run.RunID)
		if err := h.Publish(ctx, env); err != nil {
			return board, err
		}
		select {
		case <-firstEvent:
		case <-ctx.Done():
			return board, ctx.Err()
		}
		return board, nil
	})
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{
			"agent-a": {Agent: agent.Agent{ID: "agent-a"}, Engine: withRunEnd(engine)},
		}},
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) { return host, nil }),
		router,
		WithSinkBufferSize(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	lease, _ := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	defer lease.Close()

	turn, err := lease.Session().Start(context.Background(), agent.Request{
		ContextID: "caller-context",
		RunID:     "caller-run",
		Message:   inference.NewTextMessage(inference.RoleUser, "hi"),
	}, SinkSpec{
		ID: "initial",
		Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
			sinkCalls.Add(1)
			firstEventOnce.Do(func() { close(firstEvent) })
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return sinkCalls.Load() > 0 })
	if got.ContextID != "ctx" {
		t.Fatalf("ContextID = %q, want ctx", got.ContextID)
	}
	if got.RunID == "" || got.RunID == "caller-run" || got.RunID != turn.RunID() {
		t.Fatalf("RunID = %q, turn = %q", got.RunID, turn.RunID())
	}
}

func TestTurnWaitAndInterruptSemantics(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		close(started)
		select {
		case intr := <-host.Interrupts():
			<-release
			return board, agent.Interrupted(intr)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	_, session, _, _ := newTurnSession(t, engine, func(bus event.Bus) HostFactory {
		return HostFactoryFunc(func(_ context.Context, req HostRequest) (agent.Host, error) {
			return agent.HostFuncs{
				Inner:        testHost{bus: bus},
				InterruptsFn: func() <-chan agent.Interrupt { return req.Interrupts },
				AskUserFn:    req.AskUser,
			}, nil
		})
	})
	turn, err := session.Start(context.Background(), agent.Request{Message: inference.NewTextMessage(inference.RoleUser, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := turn.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want canceled", err)
	}
	first := agent.Interrupt{Cause: agent.CauseUserInput, Detail: "first"}
	if err := turn.Interrupt(first); err != nil {
		t.Fatal(err)
	}
	if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseCustom, Detail: "second"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusInterrupted || result.Cause != first.Cause {
		t.Fatalf("result = %+v", result)
	}

	const waiters = 8
	var wg sync.WaitGroup
	wg.Add(waiters)
	for range waiters {
		go func() {
			defer wg.Done()
			got, err := turn.Wait(context.Background())
			if err != nil || got != result {
				t.Errorf("Wait = (%p, %v), want (%p, nil)", got, err, result)
			}
		}()
	}
	wg.Wait()
}

func TestTurnConcurrentPromptsReplyOutOfOrder(t *testing.T) {
	seen := make(chan PromptRequested, 2)
	replies := make(chan string, 2)
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		var wg sync.WaitGroup
		for _, source := range []string{"first", "second"} {
			wg.Add(1)
			go func(source string) {
				defer wg.Done()
				reply, err := host.AskUser(ctx, agent.UserPrompt{Source: source})
				if err != nil {
					replies <- "error:" + err.Error()
					return
				}
				replies <- reply.Metadata["source"]
			}(source)
		}
		wg.Wait()
		return board, nil
	})
	_, session, _, bus := newTurnSession(t, engine, func(bus event.Bus) HostFactory {
		return HostFactoryFunc(func(_ context.Context, req HostRequest) (agent.Host, error) {
			return agent.HostFuncs{
				Inner:        testHost{bus: bus},
				InterruptsFn: func() <-chan agent.Interrupt { return req.Interrupts },
				AskUserFn:    req.AskUser,
			}, nil
		})
	})
	sub, err := bus.Subscribe(context.Background(), agent.PatternAllRuns())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	go func() {
		for env := range sub.C() {
			var requested PromptRequested
			if env.Decode(&requested) == nil {
				seen <- requested
			}
		}
	}()

	turn, err := session.Start(context.Background(), agent.Request{Message: inference.NewTextMessage(inference.RoleUser, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	a, b := <-seen, <-seen
	if err := turn.Reply(context.Background(), b.PromptID, agent.UserReply{Metadata: map[string]string{"source": b.Prompt.Source}}); err != nil {
		t.Fatal(err)
	}
	if err := turn.Reply(context.Background(), a.PromptID, agent.UserReply{Metadata: map[string]string{"source": a.Prompt.Source}}); err != nil {
		t.Fatal(err)
	}
	if err := turn.Reply(context.Background(), a.PromptID, agent.UserReply{}); !errors.Is(err, ErrPromptDuplicate) {
		t.Fatalf("duplicate Reply error = %v", err)
	}
	if err := turn.Reply(context.Background(), "missing", agent.UserReply{}); !errors.Is(err, ErrPromptUnknown) {
		t.Fatalf("unknown Reply error = %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{<-replies: true, <-replies: true}
	if !got["first"] || !got["second"] {
		t.Fatalf("replies = %#v", got)
	}
}
