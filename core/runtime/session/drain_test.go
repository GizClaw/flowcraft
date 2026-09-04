package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
)

func TestManagerIdleAndWaitIdleImmediate(t *testing.T) {
	manager, _ := newTestManager(t, time.Hour)
	if !manager.Idle() {
		t.Fatal("Idle() = false, want true with no live sessions")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v, want nil", err)
	}
}

func TestManagerWaitIdleBlocksUntilActiveTurnFinishes(t *testing.T) {
	release := make(chan struct{})
	manager, session, _, _ := newTurnSession(
		t,
		withRunEnd(blockingEngine(release)),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		},
	)

	if _, err := session.Start(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, time.Second, func() bool { return !manager.Idle() })

	waited := make(chan error, 1)
	go func() {
		waited <- manager.WaitIdle(context.Background())
	}()
	select {
	case err := <-waited:
		t.Fatalf("WaitIdle returned before turn finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitIdle did not return after turn finished")
	}
}

func TestManagerDrainRejectsNewWorkAndWaitsForActiveTurn(t *testing.T) {
	release := make(chan struct{})
	manager, session, _, _ := newTurnSession(
		t,
		withRunEnd(blockingEngine(release)),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		},
	)

	turn, err := session.Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	drained := make(chan error, 1)
	go func() {
		drained <- manager.Drain(context.Background())
	}()

	// Wait until the drain has quiesced the manager, then verify both
	// new leases and new Starts on the held lease are refused.
	eventually(t, time.Second, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.draining
	})
	if _, err := manager.Open(context.Background(), Key{
		AgentID: "agent-a", ContextID: "c2",
	}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Open during drain error = %v, want ErrManagerDraining", err)
	}
	if _, err := session.Start(context.Background(), agent.Request{}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Start during drain error = %v, want ErrManagerDraining", err)
	}
	if manager.Idle() {
		t.Fatal("Idle() = true while the active turn is still running")
	}

	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after turn finished")
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	if !manager.Idle() {
		t.Fatal("Idle() = false after drain completed")
	}

	// Drain is terminal: the manager must keep refusing new work until
	// the caller closes it.
	if _, err := manager.Open(context.Background(), Key{
		AgentID: "agent-a", ContextID: "c3",
	}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Open after drain error = %v, want ErrManagerDraining", err)
	}
	if _, err := session.Start(context.Background(), agent.Request{}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Start after drain error = %v, want ErrManagerDraining", err)
	}
}

func TestManagerDrainTimeoutKeepsSessionsAndCanBeRetried(t *testing.T) {
	release := make(chan struct{})
	manager, session, _, _ := newTurnSession(
		t,
		withRunEnd(blockingEngine(release)),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		},
	)

	if _, err := session.Start(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := manager.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain error = %v, want DeadlineExceeded", err)
	}
	if _, err := manager.Open(context.Background(), Key{
		AgentID: "agent-a", ContextID: "c2",
	}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Open after timeout error = %v, want ErrManagerDraining", err)
	}
	if _, err := session.Start(context.Background(), agent.Request{}); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("Start after timeout error = %v, want ErrManagerDraining", err)
	}

	manager.mu.Lock()
	entry := manager.entries[Key{AgentID: "agent-a", ContextID: "ctx"}]
	manager.mu.Unlock()
	if entry == nil || entry.session == nil {
		t.Fatal("session was removed despite drain timeout")
	}

	close(release)
	if err := manager.Drain(context.Background()); err != nil {
		t.Fatalf("retry Drain: %v", err)
	}
	if !manager.Idle() {
		t.Fatal("manager not idle after successful retry")
	}
}

func TestManagerDrainRejectsStartThatAlreadyAcquiredEpoch(t *testing.T) {
	manager, session, _, _ := newTurnSession(
		t,
		withRunEnd(agent.EngineFunc(func(
			context.Context, agent.Run, agent.Host, *agent.Board,
		) (*agent.Board, error) {
			return agent.NewBoard(), nil
		})),
		turnHostFactory,
	)

	// Start checks the manager's draining flag again after beginEpoch,
	// so a drain that lands in the beginEpoch -> interruptActive window
	// must still refuse the turn instead of installing it after Drain
	// observed an idle manager.
	manager.mu.Lock()
	manager.draining = true
	manager.mu.Unlock()

	if _, err := session.interruptActive(context.Background()); !errors.Is(err, ErrManagerDraining) {
		t.Fatalf("interruptActive during drain error = %v, want ErrManagerDraining", err)
	}
	if !manager.Idle() {
		t.Fatal("interruptActive leaked an activity slot after drain rejection")
	}
}
