package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

type fakeSessionCatalog struct {
	defs   []message.Definition
	closed atomic.Int64
}

func (f *fakeSessionCatalog) Get(string) (sdktool.Tool, bool) { return nil, false }
func (f *fakeSessionCatalog) Definitions() []message.Definition {
	return f.defs
}
func (f *fakeSessionCatalog) Close() error {
	f.closed.Add(1)
	return nil
}

func catalogAwareHostFactory(t *testing.T, bus event.Bus, got *sdktool.Catalog) HostFactory {
	t.Helper()
	var mu sync.Mutex
	return HostFactoryFunc(func(ctx context.Context, _ HostRequest) (agent.Host, error) {
		catalog, ok := sdktool.CatalogFromContext(ctx)
		mu.Lock()
		if ok {
			*got = catalog
		}
		mu.Unlock()
		if !ok {
			t.Error("turn context carries no session catalog")
		}
		return testHost{bus: bus}, nil
	})
}

func TestSessionCatalogProvider_AttachesOnceAndCloses(t *testing.T) {
	cat := &fakeSessionCatalog{}
	var providerCalls atomic.Int64
	provider := CatalogProviderFunc(func(context.Context, *deploy.Instance) (sdktool.Catalog, error) {
		providerCalls.Add(1)
		return cat, nil
	})

	var saw sdktool.Catalog
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine,
		func(bus event.Bus) HostFactory { return catalogAwareHostFactory(t, bus, &saw) },
		WithCatalogProvider(provider),
	)

	req := agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")}
	for i := 0; i < 2; i++ {
		turn, err := session.Start(context.Background(), req)
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	if providerCalls.Load() != 1 {
		t.Errorf("provider calls = %d, want 1 (created once per session)", providerCalls.Load())
	}
	if saw == nil || saw != sdktool.Catalog(cat) {
		t.Errorf("host saw catalog %T, want the provider's catalog", saw)
	}
}

func TestSessionCatalogProvider_CloseFoldsIntoSessionClose(t *testing.T) {
	cat := &fakeSessionCatalog{}
	_, session, _, _ := newTurnSession(t,
		agent.EngineFunc(func(
			ctx context.Context,
			run agent.Run,
			host agent.Host,
			board *agent.Board,
		) (*agent.Board, error) {
			return board, nil
		}),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		},
		WithCatalogProvider(CatalogProviderFunc(func(context.Context, *deploy.Instance) (sdktool.Catalog, error) {
			return cat, nil
		})),
	)

	// Force catalog creation, then close the session.
	if _, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if cat.closed.Load() != 1 {
		t.Errorf("catalog close calls = %d, want exactly 1", cat.closed.Load())
	}
}

func TestSessionCatalogProvider_ProviderErrorFailsStart(t *testing.T) {
	_, session, _, _ := newTurnSession(t,
		agent.EngineFunc(func(
			ctx context.Context,
			run agent.Run,
			host agent.Host,
			board *agent.Board,
		) (*agent.Board, error) {
			return board, nil
		}),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		},
		WithCatalogProvider(CatalogProviderFunc(func(context.Context, *deploy.Instance) (sdktool.Catalog, error) {
			return nil, errors.New("catalog unavailable")
		})),
	)
	_, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err == nil || err.Error() != "catalog unavailable" {
		t.Fatalf("Start error = %v, want provider error", err)
	}
}

func TestSessionCatalogProvider_NilProviderIsNoop(t *testing.T) {
	_, session, _, _ := newTurnSession(t,
		agent.EngineFunc(func(
			ctx context.Context,
			run agent.Run,
			host agent.Host,
			board *agent.Board,
		) (*agent.Board, error) {
			return board, nil
		}),
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(ctx context.Context, _ HostRequest) (agent.Host, error) {
				if _, ok := sdktool.CatalogFromContext(ctx); ok {
					t.Error("no provider configured, but a catalog reached the turn context")
				}
				return testHost{bus: bus}, nil
			})
		},
	)
	turn, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
