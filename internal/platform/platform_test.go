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
	"github.com/GizClaw/flowcraft/internal/template"
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

func TestTaskBoard_NoRealm(t *testing.T) {
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	board := p.TaskBoard()
	if board != nil {
		t.Fatal("expected nil board before Resolve")
	}
}

func TestTaskBoard_WithRealm(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	_, _ = p.Realms.Resolve(ctx)
	board := p.TaskBoard()
	if board == nil {
		t.Fatal("expected non-nil board after Resolve")
	}
}

func TestAbortAgent_NoRealm(t *testing.T) {
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	aborted, err := p.AbortAgent(context.Background(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if aborted {
		t.Fatal("expected false when no realm exists")
	}
}

func TestAbortAgent_WithRealm(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	_, _ = p.Realms.Resolve(ctx)
	aborted, err := p.AbortAgent(ctx, "nonexistent-agent")
	if err != nil {
		t.Fatal(err)
	}
	if aborted {
		t.Fatal("expected false for nonexistent agent")
	}
}

func TestRealmStats_NoRealm(t *testing.T) {
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	stats := p.RealmStats()
	if stats.RealmCount != 0 {
		t.Fatalf("expected 0 realms, got %d", stats.RealmCount)
	}
}

func TestRealmStats_WithRealm(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	_, _ = p.Realms.Resolve(ctx)
	stats := p.RealmStats()
	if stats.RealmCount != 1 {
		t.Fatalf("expected 1 realm, got %d", stats.RealmCount)
	}
}

func TestRunAgent_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	_, err := p.RunAgent(ctx, "nonexistent", &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestRunAgentStreaming(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	agent := &model.Agent{AgentID: "agent-stream", Name: "test", Type: model.AgentTypeWorkflow}
	if _, err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	req := &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--agent-stream",
		Message:   workflow.NewTextRequest("hello").Message,
	}
	ch, sub, actorKey, err := p.RunAgentStreaming(ctx, agent, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if sub == nil {
		t.Fatal("expected non-nil subscription")
	}
	defer func() { _ = sub.Close() }()

	if actorKey == "" {
		t.Fatal("expected non-empty actorKey")
	}
	result := <-ch
	if result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestInstantiateTemplate_NotFound(t *testing.T) {
	p := &Platform{
		TemplateReg: template.NewRegistry(),
	}
	result, err := p.InstantiateTemplate("nonexistent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatal("expected nil for nonexistent template")
	}
}

func TestSyncPluginSchemas_NilRegistries(t *testing.T) {
	p := &Platform{}
	p.SyncPluginSchemas()
}

func TestLoadCheckpoint_NoCheckpointStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	agent := &model.Agent{AgentID: "agent-cp", Name: "test", Type: model.AgentTypeWorkflow}
	if _, err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	p := &Platform{
		Store:  s,
		Realms: newTestProvider(t, s),
	}

	snap, err := p.LoadCheckpoint(ctx, "agent-cp")
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil {
		t.Fatal("expected nil snapshot with no checkpoint store")
	}
}
