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
	"github.com/GizClaw/flowcraft/sdk/message"
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

type sessionEngineFactory struct {
	engine agent.Engine
}

type sessionRefereeFunc func(
	context.Context,
	agent.Identity,
	*agent.Request,
	*agent.Result,
) (agent.Decision, error)

func (f sessionRefereeFunc) After(
	ctx context.Context,
	id agent.Identity,
	request *agent.Request,
	result *agent.Result,
) (agent.Decision, error) {
	return f(ctx, id, request, result)
}

func (f sessionEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: "session-revise-test"}
}

func (f sessionEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return f.engine, nil
}

type trackingBus struct {
	event.Bus
	active atomic.Int64
}

func (b *trackingBus) Subscribe(
	ctx context.Context,
	pattern event.Pattern,
	options ...event.SubOption,
) (event.Subscription, error) {
	subscription, err := b.Bus.Subscribe(ctx, pattern, options...)
	if err != nil {
		return nil, err
	}
	b.active.Add(1)
	return &trackingSubscription{
		Subscription: subscription,
		closed:       func() { b.active.Add(-1) },
	}, nil
}

type trackingSubscription struct {
	event.Subscription
	once   sync.Once
	closed func()
}

func (s *trackingSubscription) Close() error {
	err := s.Subscription.Close()
	s.once.Do(s.closed)
	return err
}

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

func revisingInstance(
	t *testing.T,
	engine agent.Engine,
	referee agent.Referee,
	committers ...agent.Committer,
) *deploy.Instance {
	t.Helper()
	registry := agent.NewRegistry()
	if err := registry.Register(sessionEngineFactory{engine: withRunEnd(engine)}); err != nil {
		t.Fatal(err)
	}
	builder := deploy.NewBuilder(registry)
	entry := deploy.AgentEntry{
		Engine: deploy.EngineEntry{Kind: "session-revise-test"},
	}
	entry.Policy.MaxRevise = 2
	built, err := builder.Build(context.Background(), deploy.Document{
		Version: deploy.VersionV1,
		Agents:  map[string]deploy.AgentEntry{"agent-a": entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = built.Close() })
	instance, ok := built.Instance("agent-a")
	if !ok {
		t.Fatal("built instance agent-a is missing")
	}
	instance.Agent.Referees = []agent.Referee{referee}
	instance.Agent.Committers = append([]agent.Committer(nil), committers...)
	return instance
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
		Message:   message.NewTextMessage(message.RoleUser, "hi"),
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

func TestSessionStartHonorsCancelledContextWhileWaitingForOldTurn(t *testing.T) {
	release := make(chan struct{})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return board, nil
	})
	makeFactory := func(bus event.Bus) HostFactory {
		return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
			return testHost{bus: bus}, nil
		})
	}
	_, session, _, _ := newTurnSession(t, engine, makeFactory)

	req := agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")}
	if _, err := session.Start(context.Background(), req); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	started := make(chan error, 1)
	go func() {
		_, err := session.Start(canceled, req)
		started <- err
	}()

	select {
	case err := <-started:
		if err == nil {
			t.Fatal("second Start succeeded with canceled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Start blocked on old turn despite canceled context")
	}
	close(release)
}

func TestSessionStartCloseRaceDetachesPostAttachCoordinator(t *testing.T) {
	memoryBus := event.NewMemoryBus()
	bus := &trackingBus{Bus: memoryBus}
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	hostEntered := make(chan struct{})
	hostRelease := make(chan struct{})
	instance := &deploy.Instance{
		Agent: agent.Agent{ID: "agent-a"},
		Engine: withRunEnd(agent.EngineFunc(func(
			context.Context, agent.Run, agent.Host, *agent.Board,
		) (*agent.Board, error) {
			t.Fatal("engine must not start after session close")
			return nil, nil
		})),
	}
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
			close(hostEntered)
			<-hostRelease
			return testHost{bus: bus}, nil
		}),
		router,
		WithSinkBufferSize(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	startResult := make(chan error, 1)
	go func() {
		_, startErr := lease.Session().Start(
			context.Background(),
			agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
			SinkSpec{
				ID: "sink",
				Sink: agent.StreamSinkFunc(func(
					context.Context, event.Envelope, agent.StreamDeltaPayload,
				) error {
					return nil
				}),
			},
		)
		startResult <- startErr
	}()
	<-hostEntered
	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.Close() }()
	eventually(t, time.Second, lease.Session().isClosing)
	close(hostRelease)
	if startErr := <-startResult; !errors.Is(startErr, ErrSessionClosed) {
		t.Fatalf("Start error = %v", startErr)
	}
	if closeErr := <-closeResult; closeErr != nil {
		t.Fatal(closeErr)
	}
	eventually(t, time.Second, func() bool { return bus.active.Load() == 0 })
	_ = lease.Close()
	_ = router.Close()
	_ = bus.Close()
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
	turn, err := session.Start(context.Background(), agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
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

func TestAuthoritativeAckCommitsOnlyFrozenPrefix(t *testing.T) {
	committed := make(chan string, 1)
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "hello world"))
		if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", "hello"); err != nil {
			return board, err
		}
		if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", " world"); err != nil {
			return board, err
		}
		select {
		case interrupt := <-host.Interrupts():
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	instance := &deploy.Instance{
		Agent: agent.Agent{
			ID: "agent-a",
			Committers: []agent.Committer{agent.CommitterFunc(func(
				_ context.Context,
				_ agent.Identity,
				_ *agent.Request,
				result *agent.Result,
			) error {
				committed <- result.Text()
				return nil
			})},
		},
		Engine: withRunEnd(engine),
	}
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		HostFactoryFunc(func(_ context.Context, request HostRequest) (agent.Host, error) {
			return agent.HostFuncs{
				Inner:        testHost{bus: bus},
				InterruptsFn: func() <-chan agent.Interrupt { return request.Interrupts },
				AskUserFn:    request.AskUser,
			}, nil
		}),
		router,
		WithSinkBufferSize(8),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	deliveries := make(chan DeliveryCursor, 2)
	turn, err := lease.Session().Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID: "authority", Sink: agent.StreamSinkFunc(func(
				_ context.Context,
				env event.Envelope,
				delta agent.StreamDeltaPayload,
			) error {
				if delta.Type == agent.StreamDeltaToken {
					cursor, cursorErr := DeliveryCursorFromEnvelope(env)
					if cursorErr != nil {
						return cursorErr
					}
					deliveries <- cursor
				}
				return nil
			}),
			Visibility: VisibilityConfirmed,
			Authority:  AuthorityAuthoritative,
			AckMode:    AckExplicit,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	first := <-deliveries
	eventually(t, time.Second, func() bool {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return turn.deliveredCursor >= first
	})
	if err := turn.Ack("authority", first); err != nil {
		t.Fatal(err)
	}
	if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != "hello world" {
		t.Fatalf("turn result = %q", result.Text())
	}
	if got := <-committed; got != "hello" {
		t.Fatalf("committed text = %q", got)
	}
}

func TestSessionAuthoritySurvivesReviseAttempts(t *testing.T) {
	var calls atomic.Int64
	committed := make(chan string, 1)
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		attempt := calls.Add(1)
		text := "first"
		if attempt == 2 {
			text = "second complete"
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, text))
		if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", text); err != nil {
			return board, err
		}
		return board, nil
	})
	refereeCalls := 0
	instance := revisingInstance(t, engine, sessionRefereeFunc(func(
		context.Context, agent.Identity, *agent.Request, *agent.Result,
	) (agent.Decision, error) {
		refereeCalls++
		return agent.Decision{Revise: refereeCalls == 1}, nil
	}), agent.CommitterFunc(func(
		_ context.Context, _ agent.Identity, _ *agent.Request, result *agent.Result,
	) error {
		committed <- result.Text()
		return nil
	}))

	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return testHost{bus: bus}, nil
		}),
		router,
		WithSinkBufferSize(8),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	var (
		mu      sync.Mutex
		tokens  []string
		runEnds int
	)
	turn, err := lease.Session().Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID: "authority",
			Sink: agent.StreamSinkFunc(func(
				_ context.Context, env event.Envelope, delta agent.StreamDeltaPayload,
			) error {
				mu.Lock()
				defer mu.Unlock()
				if delta.Type == agent.StreamDeltaToken {
					tokens = append(tokens, delta.Content)
				}
				if env.Subject == agent.SubjectRunEnd(env.RunID()) {
					runEnds++
				}
				return nil
			}),
			Visibility: VisibilityConfirmed,
			Authority:  AuthorityAuthoritative,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted || result.Attempts != 2 || result.Text() != "second complete" {
		t.Fatalf("result = %+v text=%q", result, result.Text())
	}
	if got := <-committed; got != "second complete" {
		t.Fatalf("committed text = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "first" || tokens[1] != "second complete" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if runEnds != 1 {
		t.Fatalf("logical run ends = %d, want 1", runEnds)
	}
}

func TestSessionInterruptedReviseCommitsOnlySecondAttemptPrefix(t *testing.T) {
	secondEmitted := make(chan struct{})
	committed := make(chan string, 1)
	var calls atomic.Int64
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if calls.Add(1) == 1 {
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "first-old"))
			if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", "first-old"); err != nil {
				return board, err
			}
			return board, nil
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "second-prefix remainder"))
		if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", "second-prefix"); err != nil {
			return board, err
		}
		close(secondEmitted)
		select {
		case interrupt := <-host.Interrupts():
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	refereeCalls := 0
	instance := revisingInstance(t, engine, sessionRefereeFunc(func(
		context.Context, agent.Identity, *agent.Request, *agent.Result,
	) (agent.Decision, error) {
		refereeCalls++
		return agent.Decision{Revise: refereeCalls == 1}, nil
	}), agent.CommitterFunc(func(
		_ context.Context, _ agent.Identity, _ *agent.Request, result *agent.Result,
	) error {
		committed <- result.Text()
		return nil
	}))

	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		HostFactoryFunc(func(_ context.Context, request HostRequest) (agent.Host, error) {
			return agent.HostFuncs{
				Inner: testHost{bus: bus},
				InterruptsFn: func() <-chan agent.Interrupt {
					return request.Interrupts
				},
				AskUserFn: request.AskUser,
			}, nil
		}),
		router,
		WithSinkBufferSize(8),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	releaseOld := make(chan struct{})
	secondCursor := make(chan DeliveryCursor, 1)
	turn, err := lease.Session().Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID: "authority",
			Sink: agent.StreamSinkFunc(func(
				_ context.Context, env event.Envelope, delta agent.StreamDeltaPayload,
			) error {
				if delta.Type != agent.StreamDeltaToken {
					return nil
				}
				if delta.Content == "first-old" {
					<-releaseOld
					return nil
				}
				cursor, cursorErr := DeliveryCursorFromEnvelope(env)
				if cursorErr != nil {
					return cursorErr
				}
				secondCursor <- cursor
				return nil
			}),
			Visibility: VisibilityConfirmed,
			Authority:  AuthorityAuthoritative,
			AckMode:    AckExplicit,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-secondEmitted
	close(releaseOld)
	cursor := <-secondCursor
	eventually(t, time.Second, func() bool {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return turn.deliveredCursor >= cursor
	})
	if err := turn.Ack("authority", cursor); err != nil {
		t.Fatal(err)
	}
	if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusInterrupted || result.Attempts != 2 {
		t.Fatalf("result = %+v", result)
	}
	if got := <-committed; got != "second-prefix" {
		t.Fatalf("committed text = %q, want second-prefix", got)
	}
}

func TestSessionRunEndPublishFailureDoesNotEmitLogicalSuccess(t *testing.T) {
	publishErr := &agent.RunEndPublishError{Err: errors.New("publish failed")}
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := agent.EmitStreamToken(ctx, host, run.RunID, "agent-a", "partial"); err != nil {
			return board, err
		}
		return board, publishErr
	})
	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus, agent.WithStreamIncludeAllRunEvents())
	instance := &deploy.Instance{Agent: agent.Agent{ID: "agent-a"}, Engine: engine}
	manager, err := NewManager(
		&testResolver{instances: map[string]*deploy.Instance{"agent-a": instance}},
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return testHost{bus: bus}, nil
		}),
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
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	var (
		mu       sync.Mutex
		subjects []event.Subject
	)
	detached := make(chan error, 1)
	turn, err := lease.Session().Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID: "raw",
			Sink: agent.StreamSinkFunc(func(
				_ context.Context, env event.Envelope, _ agent.StreamDeltaPayload,
			) error {
				mu.Lock()
				subjects = append(subjects, env.Subject)
				mu.Unlock()
				return nil
			}),
			OnDetach: func(err error) { detached <- err },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusFailed || !errors.Is(result.Err, publishErr) {
		t.Fatalf("result = %+v err=%v", result, result.Err)
	}
	if err := <-detached; !errors.Is(err, publishErr) {
		t.Fatalf("detach error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, subject := range subjects {
		if subject == agent.SubjectRunEnd(turn.RunID()) {
			t.Fatalf("synthetic run end emitted after publish failure: %#v", subjects)
		}
	}
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

	turn, err := session.Start(context.Background(), agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
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
