package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

type recordingSessionObserver struct {
	BaseSessionObserver

	mu     sync.Mutex
	events []string
}

func (o *recordingSessionObserver) add(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *recordingSessionObserver) OnSessionStarted(*Session, *Turn) {
	o.add("started")
}

func (o *recordingSessionObserver) OnSessionClosing(*Session) {
	o.add("closing")
}

func (o *recordingSessionObserver) OnSessionClosed(*Session, error) {
	o.add("closed")
}

func (o *recordingSessionObserver) OnTurnFinished(*Session, *Turn, *agent.Result, error) {
	o.add("turnFinished")
}

func (o *recordingSessionObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func eventsEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSessionObserverStartAndTurnFinished(t *testing.T) {
	observer := &recordingSessionObserver{}
	engine := agent.EngineFunc(func(
		context.Context, agent.Run, agent.Host, *agent.Board,
	) (*agent.Board, error) {
		return agent.NewBoard(), nil
	})
	_, session, _, _ := newTurnSession(
		t, engine, turnHostFactory, WithSessionObserver(observer))

	turn, err := session.Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := observer.snapshot(); !containsEvent(got, "started") {
		t.Fatalf("observer events after Start = %v, want started", got)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Turn.Wait() error = %v", err)
	}
	eventually(t, time.Second, func() bool {
		return eventsEqual(observer.snapshot(), "started", "turnFinished")
	})
}

func TestSessionObserverFiresOnceOnManagerClose(t *testing.T) {
	observer := &recordingSessionObserver{}
	engine := agent.EngineFunc(func(
		context.Context, agent.Run, agent.Host, *agent.Board,
	) (*agent.Board, error) {
		return agent.NewBoard(), nil
	})
	manager, _, _, _ := newTurnSession(
		t, engine, turnHostFactory, WithSessionObserver(observer))

	if err := manager.Close(); err != nil {
		t.Fatalf("Manager.Close() error = %v", err)
	}
	if got := observer.snapshot(); !eventsEqual(got, "closing", "closed") {
		t.Fatalf("observer events = %v, want [closing closed]", got)
	}
}

func TestSessionObserverFiresOnIdleReclaim(t *testing.T) {
	observer := &recordingSessionObserver{}
	resolver := &testResolver{instances: map[string]*agent.Agent{"agent-a": {}}}
	router := event.NewRouter(event.NewMemoryBus())
	t.Cleanup(func() { _ = router.Close() })
	manager, err := NewManager(
		resolver,
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
		WithIdleTimeout(15*time.Millisecond),
		WithSessionObserver(observer),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	_ = lease.Close()

	eventually(t, time.Second, func() bool {
		return manager.sessionCount() == 0 &&
			eventsEqual(observer.snapshot(), "closing", "closed")
	})
}

func TestSessionObserverCloseInterruptsAndWaitsForTurn(t *testing.T) {
	observer := &recordingSessionObserver{}
	engine := agent.EngineFunc(func(
		ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board,
	) (*agent.Board, error) {
		select {
		case interrupt := <-host.Interrupts():
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	manager, session, _, _ := newTurnSession(
		t, engine, turnHostFactory, WithSessionObserver(observer))

	turn, err := session.Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Manager.Close() error = %v", err)
	}
	if turn.State() != TurnInterrupted {
		t.Fatalf("turn state = %q, want interrupted", turn.State())
	}
	if got := observer.snapshot(); !eventsEqual(
		got, "started", "closing", "turnFinished", "closed",
	) {
		t.Fatalf("observer events = %v, want [started closing turnFinished closed]", got)
	}
}

func TestSessionObserverNotFiredOnFailedStart(t *testing.T) {
	observer := &recordingSessionObserver{}
	engine := agent.EngineFunc(func(
		context.Context, agent.Run, agent.Host, *agent.Board,
	) (*agent.Board, error) {
		return agent.NewBoard(), nil
	})
	_, session, _, _ := newTurnSession(
		t, engine,
		func(event.Bus) HostFactory {
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return nil, errdefs.NotAvailablef("host unavailable")
			})
		},
		WithSessionObserver(observer),
	)

	if _, err := session.Start(context.Background(), agent.Request{}); err == nil {
		t.Fatal("Start() error = nil, want failure")
	}
	if got := observer.snapshot(); len(got) != 0 {
		t.Fatalf("observer events = %v, want none", got)
	}
}

func TestWithSessionObserverRejectsTypedNil(t *testing.T) {
	router := event.NewRouter(event.NewMemoryBus())
	defer func() { _ = router.Close() }()
	var observer SessionObserver
	if _, err := NewManager(
		&testResolver{},
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
		WithSessionObserver(observer),
	); !errdefs.IsValidation(err) {
		t.Fatalf("NewManager() error = %v, want validation", err)
	}
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}
