package platform

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

func newTestStore(t *testing.T) model.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewSQLiteStore(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func init() {
	_ = os.Setenv("HOME", os.TempDir())
}

func newTestProvider(t *testing.T, s model.Store) *realm.SingleRealmProvider {
	t.Helper()
	pcfg := &realm.PlatformDeps{
		RunOverride: func(_ context.Context, _ *model.Agent, _ *workflow.Request) (*workflow.Result, error) {
			return &workflow.Result{
				Status: workflow.StatusCompleted,
				State:  map[string]any{"answer": "ok"},
			}, nil
		},
	}
	sbCfg := sandbox.DefaultManagerConfig()
	sbCfg.RootDir = t.TempDir()
	provider := realm.NewSingleRealmProvider(s, pcfg, sbCfg, sbCfg.IdleTimeout)
	t.Cleanup(provider.Close)
	return provider
}

func TestRunAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	agent := &model.Agent{AgentID: "agent-1", Name: "test", Type: model.AgentTypeWorkflow}
	if _, err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	req := &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--agent-1",
		Message:   workflow.NewTextRequest("hello").Message,
	}
	ch, err := p.RunAgent(ctx, "agent-1", req)
	if err != nil {
		t.Fatal(err)
	}
	result := <-ch
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Value.State["answer"] != "ok" {
		t.Fatalf("got %v", result.Value.State["answer"])
	}
}

func TestBoard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	board, err := p.Board(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if board == nil {
		t.Fatal("expected non-nil board")
	}
}

func TestBus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	bus, err := p.Bus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bus == nil {
		t.Fatal("expected non-nil bus")
	}
}
