package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

// blockingEngine returns an engine that blocks until release is closed
// or ctx is done, so tests can hold a turn open deterministically.
func blockingEngine(release <-chan struct{}) agent.Engine {
	return agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		select {
		case <-release:
			return board, nil
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
}

func TestManagerRemoveAgentBlocksNewSessions(t *testing.T) {
	manager, _ := newTestManager(t, time.Hour)

	first, err := manager.GetOrCreate(context.Background(), Key{AgentID: "agent-a", ContextID: "c1"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	defer func() { _ = first.Close() }()

	if err := manager.RemoveAgent(context.Background(), "agent-a"); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "c2"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open removed agent error = %v, want not found", err)
	}
	// Existing lease is dead after removal.
	if _, err := first.Session().Start(context.Background(), agent.Request{}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Start on removed lease error = %v, want ErrSessionClosed", err)
	}
	// Unrelated agent is unaffected.
	other, err := manager.GetOrCreate(context.Background(), Key{AgentID: "agent-b", ContextID: "c1"})
	if err != nil {
		t.Fatalf("GetOrCreate(agent-b): %v", err)
	}
	defer func() { _ = other.Close() }()
}

func TestManagerRemoveAgentIdempotentAndReopen(t *testing.T) {
	manager, _ := newTestManager(t, time.Hour)

	if err := manager.RemoveAgent(context.Background(), "agent-a"); err != nil {
		t.Fatalf("first RemoveAgent: %v", err)
	}
	if err := manager.RemoveAgent(context.Background(), "agent-a"); err != nil {
		t.Fatalf("second RemoveAgent: %v", err)
	}
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "c1"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open after RemoveAgent error = %v, want not found", err)
	}

	manager.ReopenAgent("agent-a")
	if _, err := manager.GetOrCreate(context.Background(), Key{AgentID: "agent-a", ContextID: "c1"}); err != nil {
		t.Fatalf("GetOrCreate after ReopenAgent: %v", err)
	}
}

func TestManagerRemoveAgentDrainsActiveTurn(t *testing.T) {
	release := make(chan struct{})
	manager, session, _, _ := newTurnSession(t, withRunEnd(blockingEngine(release)),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		})

	turn, err := session.Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	removed := make(chan error, 1)
	go func() {
		removed <- manager.RemoveAgent(context.Background(), "agent-a")
	}()

	// Wait until the tombstone is in place so the drain has actually
	// started; then new opens (even for another context) must be refused.
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		_, gone := manager.removed["agent-a"]
		manager.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RemoveAgent did not start draining")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "c2"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open during drain error = %v, want not found", err)
	}

	close(release)
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("RemoveAgent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveAgent did not return after turn finished")
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
}

func TestManagerRemoveAgentTimeoutKeepsSessions(t *testing.T) {
	release := make(chan struct{})
	manager, session, _, _ := newTurnSession(t, withRunEnd(blockingEngine(release)),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		})

	if _, err := session.Start(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := manager.RemoveAgent(ctx, "agent-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RemoveAgent error = %v, want DeadlineExceeded", err)
	}

	// Tombstone persists: new opens still fail.
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "c2"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open after timeout error = %v, want not found", err)
	}
	// Sessions were left intact, not deleted.
	manager.mu.Lock()
	entry := manager.entries[Key{AgentID: "agent-a", ContextID: "ctx"}]
	manager.mu.Unlock()
	if entry == nil {
		t.Fatal("session was removed despite timeout")
	}

	close(release)
	if err := manager.RemoveAgent(context.Background(), "agent-a"); err != nil {
		t.Fatalf("retry RemoveAgent: %v", err)
	}
	manager.mu.Lock()
	entry = manager.entries[Key{AgentID: "agent-a", ContextID: "ctx"}]
	manager.mu.Unlock()
	if entry != nil {
		t.Fatal("session survived successful RemoveAgent retry")
	}
}

func TestManagerRemoveAgentConcurrentOpenRace(t *testing.T) {
	manager, _ := newTestManager(t, time.Hour)

	const opens = 64
	const removes = 8
	var wg sync.WaitGroup
	for i := 0; i < opens; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			lease, err := manager.GetOrCreate(ctx, Key{
				AgentID:   "agent-a",
				ContextID: "c",
			})
			if err == nil {
				_ = lease.Close()
			}
		}(i)
	}
	for i := 0; i < removes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = manager.RemoveAgent(context.Background(), "agent-a")
		}()
	}
	wg.Wait()

	// After RemoveAgent returned, no session for the removed agent may
	// remain, and new opens must fail.
	manager.mu.Lock()
	for key := range manager.entries {
		if key.AgentID == "agent-a" {
			manager.mu.Unlock()
			t.Fatalf("leaked session for removed agent: %+v", key)
		}
	}
	manager.mu.Unlock()
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "c"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open after concurrent removal error = %v, want not found", err)
	}
}
