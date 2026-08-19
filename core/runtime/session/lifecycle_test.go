package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func turnHostFactory(bus event.Bus) HostFactory {
	return HostFactoryFunc(func(_ context.Context, request HostRequest) (agent.Host, error) {
		return agent.HostFuncs{
			Inner:        testHost{bus: bus},
			InterruptsFn: func() <-chan agent.Interrupt { return request.Interrupts },
			AskUserFn:    request.AskUser,
		}, nil
	})
}

func TestSessionConcurrentStartNeverExecutesTwoTurns(t *testing.T) {
	var running atomic.Int64
	var maximum atomic.Int64
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		current := running.Add(1)
		defer running.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case interrupt := <-host.Interrupts():
			time.Sleep(time.Millisecond)
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)

	const starts = 12
	turns := make(chan *Turn, starts)
	var wg sync.WaitGroup
	wg.Add(starts)
	for range starts {
		go func() {
			defer wg.Done()
			turn, err := session.Start(context.Background(), agent.Request{
				Message: message.NewTextMessage(message.RoleUser, "next"),
			})
			if err != nil {
				t.Errorf("Start error = %v", err)
				return
			}
			turns <- turn
		}()
	}
	wg.Wait()
	close(turns)
	for turn := range turns {
		_ = turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserCancel})
	}
	session.mu.Lock()
	active := session.active
	session.mu.Unlock()
	if active != nil {
		_ = active.Interrupt(agent.Interrupt{Cause: agent.CauseUserCancel})
		_, _ = active.Wait(context.Background())
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent executions = %d, want 1", got)
	}
}

func TestSessionStartContextCancellationClassifiesTurnCanceled(t *testing.T) {
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		<-ctx.Done()
		return board, ctx.Err()
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := session.Start(ctx, agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCanceled || turn.State() != TurnCanceled {
		t.Fatalf("result status = %q, turn state = %q", result.Status, turn.State())
	}
}

func TestPromptTimeoutAndInterruptRejectLateReply(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		promptIDs := make(chan string, 1)
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			promptCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			_, err := host.AskUser(promptCtx, agent.UserPrompt{Source: "timeout"})
			if !errors.Is(err, context.DeadlineExceeded) {
				return board, errors.New("AskUser did not time out")
			}
			return board, nil
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		go capturePromptIDs(sub, promptIDs)
		turn, err := session.Start(context.Background(), agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
		if err != nil {
			t.Fatal(err)
		}
		promptID := <-promptIDs
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := turn.Reply(context.Background(), promptID, agent.UserReply{}); !errors.Is(err, ErrPromptClosed) {
			t.Fatalf("late Reply error = %v, want closed", err)
		}
	})

	t.Run("interrupt", func(t *testing.T) {
		promptIDs := make(chan string, 1)
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			_, err := host.AskUser(ctx, agent.UserPrompt{Source: "interrupt"})
			return board, err
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		go capturePromptIDs(sub, promptIDs)
		turn, err := session.Start(context.Background(), agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
		if err != nil {
			t.Fatal(err)
		}
		promptID := <-promptIDs
		if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
			t.Fatal(err)
		}
		result, err := turn.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != agent.StatusInterrupted {
			t.Fatalf("status = %q", result.Status)
		}
		if err := turn.Reply(context.Background(), promptID, agent.UserReply{}); !errors.Is(err, ErrPromptClosed) {
			t.Fatalf("late Reply error = %v, want closed", err)
		}
	})
}

func capturePromptIDs(sub event.Subscription, ids chan<- string) {
	for envelope := range sub.C() {
		if envelope.Subject == SubjectPromptRequested(envelope.RunID()) {
			var requested PromptRequested
			if envelope.Decode(&requested) == nil {
				ids <- requested.PromptID
			}
		}
	}
}

func TestSlowSinkDetachesWithoutStoppingFastSink(t *testing.T) {
	slowRelease := make(chan struct{})
	slowDetached := make(chan error, 1)
	var fastCalls atomic.Int64
	engine := agent.EngineFunc(func(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		for i := 0; i < 16; i++ {
			envelope, err := event.NewEnvelope(ctx, agent.SubjectStreamDelta(run.RunID, "agent-a"), agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: message.TextPart{Text: "x"},
			})
			if err != nil {
				return board, err
			}
			envelope.SetRunID(run.RunID)
			if err := host.Publish(ctx, envelope); err != nil {
				return board, err
			}
		}
		select {
		case <-slowDetached:
		case <-time.After(time.Second):
			return board, errors.New("slow sink was not detached")
		}
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := session.Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID:        "slow",
			QueueSize: 1,
			Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
				<-slowRelease
				return nil
			}),
			OnDetach: func(err error) { slowDetached <- err },
		},
		SinkSpec{
			ID:        "fast",
			QueueSize: 32,
			Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
				fastCalls.Add(1)
				return nil
			}),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	close(slowRelease)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	eventually(t, time.Second, func() bool { return fastCalls.Load() > 1 })
}

func TestTurnWaitDrainsSinkThroughRunEnd(t *testing.T) {
	engineReturned := make(chan struct{})
	releaseSink := make(chan struct{})
	engine := agent.EngineFunc(func(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		defer close(engineReturned)
		for range 3 {
			envelope, err := event.NewEnvelope(ctx, agent.SubjectStreamDelta(run.RunID, "agent-a"), agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: message.TextPart{Text: "x"},
			})
			if err != nil {
				return board, err
			}
			envelope.SetRunID(run.RunID)
			if err := host.Publish(ctx, envelope); err != nil {
				return board, err
			}
		}
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	var calls atomic.Int64
	var canceledContexts atomic.Int64
	turn, err := session.Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID:        "drain",
			QueueSize: 8,
			Sink: agent.StreamSinkFunc(func(ctx context.Context, _ event.Envelope, _ agent.StreamDeltaPayload) error {
				if calls.Add(1) == 1 {
					<-releaseSink
				}
				if ctx.Err() != nil {
					canceledContexts.Add(1)
				}
				return nil
			}),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-engineReturned
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := turn.Wait(context.Background())
		waitDone <- waitErr
	}()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before the sink drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSink)
	if err := <-waitDone; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("sink calls = %d, want 3 deltas plus run-end", got)
	}
	if got := canceledContexts.Load(); got != 0 {
		t.Fatalf("sink received %d canceled contexts", got)
	}
}

func TestTurnRecordsFinalizeTimeoutWhenEngineOmitsRunEnd(t *testing.T) {
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		return board, nil
	})
	bus := event.NewMemoryBus()
	router := event.NewRouter(bus)
	instance := &agent.Agent{ID: "agent-a", Engine: engine}
	manager, err := NewManager(
		&testResolver{instances: map[string]*agent.Agent{"agent-a": instance}},
		turnHostFactory(bus),
		router,
		WithIdleTimeout(time.Minute),
		WithSinkBufferSize(8),
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

	detached := make(chan error, 1)
	turn, err := lease.Session().Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID:        "drain",
			QueueSize: 8,
			Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
				return nil
			}),
			OnDetach: func(err error) { detached <- err },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultAttemptDrainTimeout+2*time.Second)
	defer cancel()
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.State == nil || result.State[finalizeErrorStateKey] == nil {
		t.Fatalf("result state missing %q: %#v", finalizeErrorStateKey, result.State)
	}
	select {
	case detachErr := <-detached:
		if !errdefs.IsTimeout(detachErr) {
			t.Fatalf("sink detach error = %v, want timeout", detachErr)
		}
	case <-time.After(time.Second):
		t.Fatal("sink was not detached after missing run-end")
	}
}

func TestTurnWaitDoesNotBlockForeverOnStuckSink(t *testing.T) {
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	sinkEntered := make(chan struct{})
	releaseSink := make(chan struct{})
	detached := make(chan error, 1)
	var enteredOnce sync.Once
	turn, err := session.Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID:              "blocked",
			DeliveryTimeout: 30 * time.Millisecond,
			Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
				enteredOnce.Do(func() { close(sinkEntered) })
				<-releaseSink
				return nil
			}),
			OnDetach: func(err error) { detached <- err },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-sinkEntered

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, waitErr := turn.Wait(waitCtx)
	close(releaseSink)
	if waitErr != nil {
		_, _ = turn.Wait(context.Background())
		t.Fatalf("Wait remained blocked by sink delivery: %v", waitErr)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	select {
	case detachErr := <-detached:
		if !errors.Is(detachErr, context.DeadlineExceeded) {
			t.Fatalf("detach error = %v, want deadline exceeded", detachErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out sink was not detached")
	}
}

func TestTurnWaitAllowsSlowSinkWithinDeliveryTimeout(t *testing.T) {
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	detached := make(chan error, 1)
	var calls atomic.Int64
	turn, err := session.Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		SinkSpec{
			ID:              "slow-within-budget",
			DeliveryTimeout: 200 * time.Millisecond,
			Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
				time.Sleep(20 * time.Millisecond)
				calls.Add(1)
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
	if result.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("sink calls = %d, want run-end delivery", got)
	}
	select {
	case detachErr := <-detached:
		if detachErr != nil {
			t.Fatalf("detach error = %v, want nil", detachErr)
		}
	case <-time.After(time.Second):
		t.Fatal("sink did not detach after run-end delivery")
	}
}

func TestManagerCloseInterruptsAndWaitsForActiveTurn(t *testing.T) {
	finalized := make(chan struct{})
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		defer close(finalized)
		select {
		case interrupt := <-host.Interrupts():
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	manager, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := session.Start(context.Background(), agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finalized:
	default:
		t.Fatal("Manager.Close returned before engine finalized")
	}
	if turn.State() != TurnInterrupted {
		t.Fatalf("turn state = %q, want interrupted", turn.State())
	}
	if _, err := session.Start(context.Background(), agent.Request{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Start after close error = %v", err)
	}
}

func TestManagerCloseWaitsForReplacementFinalization(t *testing.T) {
	started := make(chan struct{})
	interrupted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		once.Do(func() { close(started) })
		interrupt := <-host.Interrupts()
		close(interrupted)
		<-release
		return board, agent.Interrupted(interrupt)
	})
	manager, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	if _, err := session.Start(context.Background(), agent.Request{}); err != nil {
		t.Fatal(err)
	}
	<-started

	replacementDone := make(chan error, 1)
	go func() {
		_, err := session.Start(context.Background(), agent.Request{})
		replacementDone <- err
	}()
	<-interrupted

	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Manager.Close returned before finalization: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-replacementDone; !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("replacement Start error = %v, want session closed", err)
	}
}

func TestHostFactoryNilAndErrorPreventExecution(t *testing.T) {
	var executions atomic.Int64
	engine := agent.EngineFunc(func(context.Context, agent.Run, agent.Host, *agent.Board) (*agent.Board, error) {
		executions.Add(1)
		return agent.NewBoard(), nil
	})
	for _, test := range []struct {
		name    string
		factory func(event.Bus) HostFactory
	}{
		{
			name: "nil",
			factory: func(event.Bus) HostFactory {
				return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) { return nil, nil })
			},
		},
		{
			name: "error",
			factory: func(event.Bus) HostFactory {
				return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
					return nil, errdefs.NotAvailablef("host unavailable")
				})
			},
		},
		{
			name: "typed nil",
			factory: func(event.Bus) HostFactory {
				return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
					return (*testHost)(nil), nil
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, session, _, _ := newTurnSession(t, engine, test.factory)
			if _, err := session.Start(context.Background(), agent.Request{}); err == nil {
				t.Fatal("Start error = nil")
			}
		})
	}
	if executions.Load() != 0 {
		t.Fatalf("engine executions = %d, want 0", executions.Load())
	}
}

type capturedPromptEvent struct {
	kind     string // "requested" or "resolved"
	promptID string
	status   PromptStatus
}

func capturePromptLifecycle(sub event.Subscription) chan capturedPromptEvent {
	events := make(chan capturedPromptEvent, 32)
	go func() {
		for env := range sub.C() {
			switch env.Subject {
			case SubjectPromptRequested(env.RunID()):
				var req PromptRequested
				if env.Decode(&req) == nil {
					events <- capturedPromptEvent{kind: "requested", promptID: req.PromptID}
				}
			case SubjectPromptResolved(env.RunID()):
				var res PromptResolved
				if env.Decode(&res) == nil {
					events <- capturedPromptEvent{
						kind:     "resolved",
						promptID: res.PromptID,
						status:   res.Status,
					}
				}
			}
		}
	}()
	return events
}

func waitForPromptEvent(t *testing.T, events chan capturedPromptEvent, want capturedPromptEvent) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case got := <-events:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for prompt event %+v", want)
		}
	}
}

func waitForAnyPromptEvent(t *testing.T, events chan capturedPromptEvent) capturedPromptEvent {
	t.Helper()
	select {
	case got := <-events:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first prompt event")
		return capturedPromptEvent{}
	}
}

func TestPromptResolutionEvents(t *testing.T) {
	t.Run("replied", func(t *testing.T) {
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			reply, err := host.AskUser(ctx, agent.UserPrompt{Source: "replied"})
			if err != nil {
				return board, err
			}
			if len(reply.Parts) == 0 {
				return board, errors.New("AskUser reply missing parts")
			}
			return board, nil
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		events := capturePromptLifecycle(sub)
		turn, err := session.Start(context.Background(), agent.Request{
			Message: message.NewTextMessage(message.RoleUser, "hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req := waitForAnyPromptEvent(t, events)
		if req.kind != "requested" {
			t.Fatalf("first prompt event = %+v, want requested", req)
		}
		if err := turn.Reply(context.Background(), req.promptID, agent.UserReply{
			Parts: []message.Part{message.TextPart{Text: "ok"}},
		}); err != nil {
			t.Fatal(err)
		}
		waitForPromptEvent(t, events, capturedPromptEvent{
			kind:     "resolved",
			promptID: req.promptID,
			status:   PromptReplied,
		})
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			promptCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			_, err := host.AskUser(promptCtx, agent.UserPrompt{Source: "expired"})
			return board, err
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		events := capturePromptLifecycle(sub)
		turn, err := session.Start(context.Background(), agent.Request{
			Message: message.NewTextMessage(message.RoleUser, "hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req := waitForAnyPromptEvent(t, events)
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForPromptEvent(t, events, capturedPromptEvent{
			kind:     "resolved",
			promptID: req.promptID,
			status:   PromptExpired,
		})
	})

	t.Run("interrupted", func(t *testing.T) {
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			_, err := host.AskUser(ctx, agent.UserPrompt{Source: "interrupted"})
			return board, err
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		events := capturePromptLifecycle(sub)
		turn, err := session.Start(context.Background(), agent.Request{
			Message: message.NewTextMessage(message.RoleUser, "hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req := waitForAnyPromptEvent(t, events)
		if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
			t.Fatal(err)
		}
		result, err := turn.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != agent.StatusInterrupted {
			t.Fatalf("status = %q, want interrupted", result.Status)
		}
		waitForPromptEvent(t, events, capturedPromptEvent{
			kind:     "resolved",
			promptID: req.promptID,
			status:   PromptInterrupted,
		})
	})

	t.Run("closed", func(t *testing.T) {
		askDone := make(chan error, 1)
		release := make(chan struct{})
		engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			go func() {
				_, err := host.AskUser(ctx, agent.UserPrompt{Source: "closed"})
				askDone <- err
			}()
			<-release
			return board, nil
		})
		_, session, _, bus := newTurnSession(t, engine, turnHostFactory)
		sub, _ := bus.Subscribe(context.Background(), agent.PatternAllRuns())
		defer func() { _ = sub.Close() }()
		events := capturePromptLifecycle(sub)
		turn, err := session.Start(context.Background(), agent.Request{
			Message: message.NewTextMessage(message.RoleUser, "hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req := waitForAnyPromptEvent(t, events)
		close(release)
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForPromptEvent(t, events, capturedPromptEvent{
			kind:     "resolved",
			promptID: req.promptID,
			status:   PromptClosed,
		})
		if err := <-askDone; !errors.Is(err, ErrPromptClosed) {
			t.Fatalf("AskUser error = %v, want closed", err)
		}
	})
}
