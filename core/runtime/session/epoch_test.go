package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

type stubToolSession struct{}

func (stubToolSession) Get(string) (tool.Tool, bool)          { return nil, false }
func (stubToolSession) Definitions() []message.ToolDefinition { return nil }
func (stubToolSession) Require(...string)                     {}
func (stubToolSession) Select(...string)                      {}
func (stubToolSession) RecordCall(message.ToolCall)           {}
func (stubToolSession) AdvanceTurn()                          {}
func (stubToolSession) Search(context.Context, string, int) ([]tool.SearchHit, error) {
	return nil, nil
}
func (stubToolSession) SearchWithLoad(context.Context, string, int) ([]tool.SearchHit, error) {
	return nil, nil
}
func (stubToolSession) Load(context.Context) error { return nil }
func (stubToolSession) EnsureLoaded(context.Context, ...string) error {
	return nil
}

func TestManagerEpochSwapRetiresWhenReferencesDrain(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)

	deps, release, err := manager.beginEpoch()
	if err != nil {
		t.Fatalf("beginEpoch: %v", err)
	}
	if deps.Epoch != 1 {
		t.Fatalf("beginEpoch epoch = %d, want 1", deps.Epoch)
	}

	var retired []uint64
	next := Deps{
		Resolver:    manager.deps.Resolver,
		HostFactory: manager.deps.HostFactory,
		Epoch:       0, // SwapDeps assigns the next epoch
	}
	if err := manager.SwapDeps(next, func(epoch uint64, _ Deps) {
		retired = append(retired, epoch)
	}); err != nil {
		t.Fatalf("SwapDeps: %v", err)
	}
	if len(retired) != 0 {
		t.Fatalf("onRetired fired while refs outstanding: %v", retired)
	}

	release()
	if len(retired) != 1 || retired[0] != 1 {
		t.Fatalf("onRetired = %v, want [1] after refs drained", retired)
	}

	// A second release is a no-op and must not fire the hook again.
	release()
	if len(retired) != 1 {
		t.Fatalf("onRetired fired twice: %v", retired)
	}
}

func TestManagerEpochSwapRetiresImmediatelyWhenUnreferenced(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)

	var retired []uint64
	next := Deps{
		Resolver:    manager.deps.Resolver,
		HostFactory: manager.deps.HostFactory,
	}
	if err := manager.SwapDeps(next, func(epoch uint64, _ Deps) {
		retired = append(retired, epoch)
	}); err != nil {
		t.Fatalf("SwapDeps: %v", err)
	}
	if len(retired) != 1 || retired[0] != 1 {
		t.Fatalf("onRetired = %v, want [1] (no refs outstanding)", retired)
	}
	if manager.deps.Epoch != 2 {
		t.Fatalf("current epoch = %d, want 2", manager.deps.Epoch)
	}
}

func TestManagerSwapDepsRejectsInvalid(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)
	if err := manager.SwapDeps(Deps{}, nil); !errdefs.IsValidation(err) {
		t.Fatalf("SwapDeps(empty) error = %v, want validation", err)
	}
	if err := manager.SwapDeps(Deps{
		Resolver: manager.deps.Resolver,
	}, nil); !errdefs.IsValidation(err) {
		t.Fatalf("SwapDeps(no host) error = %v, want validation", err)
	}
}

func TestManagerEpochSwapAfterCloseRejected(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := manager.SwapDeps(manager.deps, nil); !errdefs.IsNotAvailable(err) {
		t.Fatalf("SwapDeps after close error = %v, want not available", err)
	}
}

// TestSessionCatalogInvalidatesAcrossEpochs verifies that a session
// builds a fresh tool catalog when its next Start runs on a new epoch
// instead of reusing the previous epoch's cached view.
func TestSessionCatalogInvalidatesAcrossEpochs(t *testing.T) {
	resolver := &testResolver{instances: map[string]*agent.Agent{
		"agent-a": {},
	}}
	router := event.NewRouter(event.NewMemoryBus())
	t.Cleanup(func() { _ = router.Close() })

	var builds atomic.Int64
	provider := CatalogProviderFunc(func(
		context.Context,
		*agent.Agent,
	) (tool.Session, error) {
		builds.Add(1)
		return stubToolSession{}, nil
	})
	manager, err := NewManager(
		resolver,
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
		WithIdleTimeout(time.Minute),
		WithCatalogProvider(provider),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	lease, err := manager.Open(context.Background(), Key{
		AgentID: "agent-a", ContextID: "ctx",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	s := lease.Session()

	first, _, err := manager.beginEpoch()
	if err != nil {
		t.Fatalf("beginEpoch: %v", err)
	}
	catalog, err := s.catalogFor(context.Background(), first, &agent.Agent{})
	if err != nil {
		t.Fatalf("catalogFor(epoch 1): %v", err)
	}
	if catalog == nil || builds.Load() != 1 {
		t.Fatalf("epoch 1 catalog build count = %d, want 1", builds.Load())
	}
	// Same epoch reuses the cache.
	if _, err := s.catalogFor(context.Background(), first, &agent.Agent{}); err != nil {
		t.Fatalf("catalogFor(epoch 1, again): %v", err)
	}
	if builds.Load() != 1 {
		t.Fatalf("same-epoch build count = %d, want 1 (cached)", builds.Load())
	}

	second := Deps{
		Resolver:        manager.deps.Resolver,
		HostFactory:     manager.deps.HostFactory,
		CatalogProvider: provider,
		Epoch:           2,
	}
	if _, err := s.catalogFor(context.Background(), second, &agent.Agent{}); err != nil {
		t.Fatalf("catalogFor(epoch 2): %v", err)
	}
	if builds.Load() != 2 {
		t.Fatalf("cross-epoch build count = %d, want 2", builds.Load())
	}
}
