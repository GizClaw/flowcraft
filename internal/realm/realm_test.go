package realm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

func newRuntimeSandboxConfig(t *testing.T) sandbox.ManagerConfig {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := sandbox.DefaultManagerConfig()
	cfg.RootDir = root
	cfg.IdleTimeout = time.Second
	return cfg
}

func TestSingleRealmProvider_Get_ReturnsSingleton(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	rt1, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rt2, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rt1 != rt2 {
		t.Fatal("expected same singleton runtime instance")
	}
}

func TestSingleRealmProvider_Current_ReturnsCreatedRuntime(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	rt1, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rt2, ok := mgr.Current()
	if !ok {
		t.Fatal("expected current runtime to be available")
	}
	if rt1 != rt2 {
		t.Fatal("expected current runtime to match created runtime")
	}
}

func TestRuntime_GetOrCreateActor_PerAgentSingleton(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	a1 := rt.GetOrCreateActor("agent1")
	a2 := rt.GetOrCreateActor("agent1")
	if a1 != a2 {
		t.Fatal("same agent should reuse actor")
	}

	b1 := rt.GetOrCreateActor("agent2")
	if b1 == a1 {
		t.Fatal("different agents should not share actor")
	}
}

func TestRuntime_SendToAgent(t *testing.T) {
	store := newMockStore()
	store.apps["agent1"] = &model.Agent{AgentID: "agent1", Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)
	pcfg := &PlatformDeps{
		RunOverride: func(_ context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error) {
			return mockResult("reply:" + agent.AgentID + ":" + req.Message.Content()), nil
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	req := &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--agent1",
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, "hello"),
	}
	done := rt.SendToAgent(context.Background(), store.apps["agent1"], req)
	result := <-done
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := result.Value.Text(); got != "reply:agent1:hello" {
		t.Fatalf("unexpected answer: %q", got)
	}
}

func TestRuntime_CloseRejectsNewSend(t *testing.T) {
	store := newMockStore()
	store.apps["agent1"] = &model.Agent{AgentID: "agent1", Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	rt.Close()

	req := &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--agent1",
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, "hello"),
	}
	result := <-rt.SendToAgent(context.Background(), store.apps["agent1"], req)
	if result.Err != ErrActorStopped {
		t.Fatalf("SendToAgent after Close err = %v, want %v", result.Err, ErrActorStopped)
	}
}

func TestRuntime_GetOrCreateActor_InjectsRuntimeContext(t *testing.T) {
	store := newMockStore()
	store.apps["agent1"] = &model.Agent{AgentID: "agent1", Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)
	pcfg := &PlatformDeps{
		RunOverride: func(ctx context.Context, _ *model.Agent, _ *workflow.Request) (*workflow.Result, error) {
			if runtimeID := model.RuntimeIDFrom(ctx); runtimeID != "owner" {
				t.Fatalf("runtime id = %q, want %q", runtimeID, "owner")
			}
			if model.SandboxHandleFrom(ctx) == nil {
				t.Fatal("expected sandbox handle in actor context")
			}
			if kanban.KanbanFrom(ctx) == nil {
				t.Fatal("expected kanban instance in actor context")
			}
			if _, ok := kanban.TaskBoardFrom(ctx); !ok {
				t.Fatal("expected task board in actor context")
			}
			return &workflow.Result{Status: workflow.StatusCompleted}, nil
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	req := &workflow.Request{
		RuntimeID: "owner",
		ContextID: "owner--agent1",
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, "hello"),
	}
	done := rt.SendToAgent(context.Background(), store.apps["agent1"], req)
	result := <-done
	if result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestRuntimeAgentExecutor_ExecutesCallbackChainAndPreservesTaskContext(t *testing.T) {
	store := newMockStore()
	store.apps["builder"] = &model.Agent{AgentID: "builder", Type: model.AgentTypeWorkflow}
	store.apps[model.CoPilotAgentID] = &model.Agent{AgentID: model.CoPilotAgentID, Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)

	type runCall struct {
		agentID        string
		conversationID string
		query          string
	}
	var (
		mu    sync.Mutex
		calls []runCall
	)
	pcfg := &PlatformDeps{
		RunOverride: func(_ context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error) {
			mu.Lock()
			calls = append(calls, runCall{
				agentID:        agent.AgentID,
				conversationID: req.ContextID,
				query:          req.Message.Content(),
			})
			mu.Unlock()
			switch agent.AgentID {
			case "builder":
				return mockResultWithRunID("已创建 RAG 应用", "run-builder-1"), nil
			case model.CoPilotAgentID:
				return mockResult("已处理 callback"), nil
			default:
				t.Fatalf("unexpected agent: %s", agent.AgentID)
				return nil, nil
			}
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	sub, err := rt.Bus().Subscribe(context.Background(), event.EventFilter{
		Types: []event.EventType{
			event.EventType(kanban.EventTaskClaimed),
			event.EventType(kanban.EventTaskCompleted),
			event.EventType(kanban.EventCallbackStart),
			event.EventType(kanban.EventCallbackDone),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()

	card := rt.Board().Produce("task", model.CoPilotAgentID, map[string]any{
		"query":           "为用户创建一个 RAG 应用",
		"target_agent_id": "builder",
		"user_query":      "帮我创建一个 RAG 应用",
		"dispatch_note":   "完成后总结关键步骤并回复用户",
	}, kanban.WithConsumer("builder"))

	exec := &kanbanAgentExecutor{realm: rt}
	if err := exec.ExecuteTask(context.Background(), "u1", "builder", card, "为用户创建一个 RAG 应用", nil); err != nil {
		t.Fatal(err)
	}

	doneCard, err := rt.Kanban().GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneCard.Status != kanban.CardDone {
		t.Fatalf("card status = %s, want %s", doneCard.Status, kanban.CardDone)
	}

	payload := kanban.PayloadMap(doneCard.Payload)
	for key, want := range map[string]string{
		"query":           "为用户创建一个 RAG 应用",
		"target_agent_id": "builder",
		"user_query":      "帮我创建一个 RAG 应用",
		"dispatch_note":   "完成后总结关键步骤并回复用户",
		"output":          "已创建 RAG 应用",
		"run_id":          "run-builder-1",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Fatalf("payload[%q] = %q, want %q", key, got, want)
		}
	}

	mu.Lock()
	if len(calls) != 2 {
		mu.Unlock()
		t.Fatalf("call count = %d, want 2", len(calls))
	}
	if calls[0].agentID != "builder" || calls[0].conversationID != "owner--builder" {
		mu.Unlock()
		t.Fatalf("unexpected child call: %+v", calls[0])
	}
	if calls[1].agentID != model.CoPilotAgentID || calls[1].conversationID != "owner--"+model.CoPilotAgentID {
		mu.Unlock()
		t.Fatalf("unexpected callback call: %+v", calls[1])
	}
	if !kanban.IsCallbackMessage(calls[1].query) {
		mu.Unlock()
		t.Fatalf("expected callback query, got:\n%s", calls[1].query)
	}
	if !strings.Contains(calls[1].query, `task_context(card_id="`+card.ID+`")`) {
		mu.Unlock()
		t.Fatalf("callback missing task_context card id, got:\n%s", calls[1].query)
	}
	if !strings.Contains(calls[1].query, "Summary: 已创建 RAG 应用") {
		mu.Unlock()
		t.Fatalf("callback missing summary, got:\n%s", calls[1].query)
	}
	mu.Unlock()

	var eventTypes []event.EventType
	timeout := time.After(2 * time.Second)
	for len(eventTypes) < 4 {
		select {
		case ev := <-sub.Events():
			eventTypes = append(eventTypes, ev.Type)
		case <-timeout:
			t.Fatalf("timed out waiting for callback events, got %v", eventTypes)
		}
	}
	wantTypes := []event.EventType{
		event.EventType(kanban.EventTaskClaimed),
		event.EventType(kanban.EventTaskCompleted),
		event.EventType(kanban.EventCallbackStart),
		event.EventType(kanban.EventCallbackDone),
	}
	for i, want := range wantTypes {
		if eventTypes[i] != want {
			t.Fatalf("event[%d] = %s, want %s", i, eventTypes[i], want)
		}
	}
}

func TestRuntimeAgentExecutor_ContinuesAfterParentContextCanceled(t *testing.T) {
	store := newMockStore()
	store.apps["builder"] = &model.Agent{AgentID: "builder", Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)
	pcfg := &PlatformDeps{
		RunOverride: func(_ context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error) {
			if agent.AgentID != "builder" {
				t.Fatalf("unexpected agent: %s", agent.AgentID)
			}
			return mockResult("done:" + req.Message.Content()), nil
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	card := rt.Board().Produce("task", model.CoPilotAgentID, map[string]any{
		"query":           "后台继续执行",
		"target_agent_id": "builder",
	}, kanban.WithConsumer("builder"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exec := &kanbanAgentExecutor{realm: rt}
	if err := exec.ExecuteTask(ctx, "u1", "builder", card, "后台继续执行", nil); err != nil {
		t.Fatal(err)
	}

	doneCard, err := rt.Kanban().GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneCard.Status != kanban.CardDone {
		t.Fatalf("card status = %s, want %s", doneCard.Status, kanban.CardDone)
	}
	payload := kanban.PayloadMap(doneCard.Payload)
	if got, _ := payload["output"].(string); got != "done:后台继续执行" {
		t.Fatalf("payload output = %q", got)
	}
}

func TestRuntimeAgentExecutor_CallbackFailurePublishesErrorDone(t *testing.T) {
	store := newMockStore()
	store.apps["builder"] = &model.Agent{AgentID: "builder", Type: model.AgentTypeWorkflow}
	store.apps[model.CoPilotAgentID] = &model.Agent{AgentID: model.CoPilotAgentID, Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)

	pcfg := &PlatformDeps{
		RunOverride: func(_ context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error) {
			switch agent.AgentID {
			case "builder":
				return mockResult("子任务完成"), nil
			case model.CoPilotAgentID:
				return nil, fmt.Errorf("callback failed for %s", req.ContextID)
			default:
				t.Fatalf("unexpected agent: %s", agent.AgentID)
				return nil, nil
			}
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	sub, err := rt.Bus().Subscribe(context.Background(), event.EventFilter{
		Types: []event.EventType{
			event.EventType(kanban.EventTaskCompleted),
			event.EventType(kanban.EventCallbackStart),
			event.EventType(kanban.EventCallbackDone),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()

	card := rt.Board().Produce("task", model.CoPilotAgentID, map[string]any{
		"query":           "执行但 callback 失败",
		"target_agent_id": "builder",
	}, kanban.WithConsumer("builder"))

	exec := &kanbanAgentExecutor{realm: rt}
	if err := exec.ExecuteTask(context.Background(), "u1", "builder", card, "执行但 callback 失败", nil); err != nil {
		t.Fatal(err)
	}

	var gotDone kanban.CallbackDonePayload
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type != event.EventType(kanban.EventCallbackDone) {
				continue
			}
			payload, ok := ev.Payload.(kanban.CallbackDonePayload)
			if !ok {
				t.Fatalf("callback_done payload type = %T", ev.Payload)
			}
			gotDone = payload
			if gotDone.Error == "" {
				t.Fatal("expected callback_done.error to be set")
			}
			if gotDone.AgentID != model.CoPilotAgentID {
				t.Fatalf("callback_done agent_id = %q", gotDone.AgentID)
			}
			return
		case <-timeout:
			t.Fatal("timed out waiting for callback_done")
		}
	}
}

func TestRuntime_RestoreKanbanFromStore(t *testing.T) {
	store := newMockStore()
	store.kanbanCards["owner"] = []*model.KanbanCard{
		{
			KanbanCardModel: kanban.KanbanCardModel{
				ID:            "card-1",
				RuntimeID:     "owner",
				Type:          "task",
				Status:        "done",
				Producer:      "copilot",
				Consumer:      "builder",
				TargetAgentID: "builder",
				Query:         "build an app",
				Output:        "done",
				CreatedAt:     time.Now().Add(-time.Minute),
				UpdatedAt:     time.Now(),
			},
		},
		{
			KanbanCardModel: kanban.KanbanCardModel{
				ID:            "card-2",
				RuntimeID:     "owner",
				Type:          "task",
				Status:        "pending",
				Producer:      "copilot",
				Consumer:      "*",
				TargetAgentID: "coder",
				Query:         "fix a bug",
				CreatedAt:     time.Now(),
				UpdatedAt:     time.Now(),
			},
		},
	}
	cfg := newRuntimeSandboxConfig(t)

	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	cards := rt.Board().Cards()
	if len(cards) != 2 {
		t.Fatalf("expected 2 restored cards, got %d", len(cards))
	}

	cardByID := make(map[string]kanban.CardInfo)
	for _, c := range cards {
		cardByID[c.ID] = c
	}
	if c, ok := cardByID["card-1"]; !ok {
		t.Fatal("card-1 not restored")
	} else if c.Status != "done" {
		t.Fatalf("card-1 status = %q, want done", c.Status)
	}
	if c, ok := cardByID["card-2"]; !ok {
		t.Fatal("card-2 not restored")
	} else if c.Status != "pending" {
		t.Fatalf("card-2 status = %q, want pending", c.Status)
	}
}

func TestRuntime_EmptyStoreNoRestore(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)

	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	cards := rt.Board().Cards()
	if len(cards) != 0 {
		t.Fatalf("expected 0 cards for empty store, got %d", len(cards))
	}
}

func TestRuntime_RestoreKanbanNilStore(t *testing.T) {
	cfg := newRuntimeSandboxConfig(t)

	rt, err := NewRealm(context.Background(), "owner", nil, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	cards := rt.Board().Cards()
	if len(cards) != 0 {
		t.Fatalf("expected 0 cards for nil store, got %d", len(cards))
	}
}

func TestRuntimeAgentExecutor_SchedulerProducerSkipsCallback(t *testing.T) {
	store := newMockStore()
	store.apps["builder"] = &model.Agent{AgentID: "builder", Type: model.AgentTypeWorkflow}
	cfg := newRuntimeSandboxConfig(t)

	var callCount int
	pcfg := &PlatformDeps{
		RunOverride: func(_ context.Context, _ *model.Agent, _ *workflow.Request) (*workflow.Result, error) {
			callCount++
			return mockResult("done"), nil
		},
	}

	rt, err := NewRealm(context.Background(), "owner", store, pcfg, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	card := rt.Board().Produce("task", "scheduler", map[string]any{
		"query":           "scheduled task",
		"target_agent_id": "builder",
	}, kanban.WithConsumer("builder"))

	exec := &kanbanAgentExecutor{realm: rt}
	if err := exec.ExecuteTask(context.Background(), "owner", "builder", card, "scheduled task", nil); err != nil {
		t.Fatal(err)
	}

	doneCard, err := rt.Kanban().GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneCard.Status != kanban.CardDone {
		t.Fatalf("card status = %s, want done", doneCard.Status)
	}
	if callCount != 1 {
		t.Fatalf("expected exactly 1 runner call (no callback), got %d", callCount)
	}
}

func TestRealm_Accessors(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "test-realm", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if rt.ID() != "test-realm" {
		t.Fatalf("ID() = %q, want test-realm", rt.ID())
	}
	if rt.Bus() == nil {
		t.Fatal("Bus() should not be nil")
	}
	if rt.Board() == nil {
		t.Fatal("Board() should not be nil")
	}
	if rt.Deps() == nil {
		t.Fatal("Deps() should not be nil")
	}
	if rt.Runtime() == nil {
		t.Fatal("Runtime() should not be nil")
	}
}

func TestRealm_Stats(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	stats := rt.Stats()
	if stats.RealmID != "owner" {
		t.Fatalf("RealmID = %q, want owner", stats.RealmID)
	}
	if stats.ActorCount != 0 {
		t.Fatalf("ActorCount = %d, want 0", stats.ActorCount)
	}

	rt.GetOrCreateActor("agent1")
	rt.GetOrCreateActor("agent2")

	stats = rt.Stats()
	if stats.ActorCount != 2 {
		t.Fatalf("ActorCount = %d, want 2", stats.ActorCount)
	}
}

func TestRealm_Actor(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	_, ok := rt.Actor("nonexistent")
	if ok {
		t.Fatal("expected Actor() to return false for nonexistent")
	}

	rt.GetOrCreateActor("agent1")
	actor, ok := rt.Actor("agent1")
	if !ok || actor == nil {
		t.Fatal("expected Actor() to return the created actor")
	}
}

func TestRealm_AbortActor(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if rt.AbortActor("nonexistent") {
		t.Fatal("expected AbortActor to return false for nonexistent actor")
	}

	rt.GetOrCreateActor("agent1")
	rt.AbortActor("agent1")
}

func TestAgentActor_Accessors(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	rt, err := NewRealm(context.Background(), "owner", store, &PlatformDeps{}, cfg, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	actor := rt.GetOrCreateActor("agent1",
		WithInboxSize(5),
		WithSource("test"),
	)

	if actor.ActorKey() != "agent1" {
		t.Fatalf("ActorKey() = %q, want agent1", actor.ActorKey())
	}
	if actor.RealmID() != "owner" {
		t.Fatalf("RealmID() = %q, want owner", actor.RealmID())
	}
	if actor.AgentID() != "agent1" {
		t.Fatalf("AgentID() = %q, want agent1", actor.AgentID())
	}
	if actor.Source() != "test" {
		t.Fatalf("Source() = %q, want test", actor.Source())
	}
	if actor.Bus() == nil {
		t.Fatal("Bus() should not be nil")
	}
	if actor.IsRunning() {
		t.Fatal("expected actor not to be running")
	}
	if actor.IsPersistent() {
		t.Fatal("expected actor not to be persistent by default")
	}
	if actor.LastActive().IsZero() {
		t.Fatal("expected non-zero LastActive")
	}
}

func TestSingleRealmProvider_Stats(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	stats := mgr.Stats()
	if stats.RealmCount != 0 {
		t.Fatalf("expected 0 realms before Get, got %d", stats.RealmCount)
	}

	_, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	stats = mgr.Stats()
	if stats.RealmCount != 1 {
		t.Fatalf("expected 1 realm after Get, got %d", stats.RealmCount)
	}
	if stats.Current == nil {
		t.Fatal("expected non-nil Current stats")
	}
}

func TestSingleRealmProvider_OnRealmCreated(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	var called bool
	mgr.OnRealmCreated(func(r *Realm) {
		called = true
	})

	_, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected OnRealmCreated callback to fire")
	}
}

func TestSingleRealmProvider_Close(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	_, err := mgr.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	mgr.Close()
	_, ok := mgr.Current()
	if ok {
		t.Fatal("expected Current() to return false after Close")
	}
}

func TestSingleRealmProvider_Get_Concurrent(t *testing.T) {
	store := newMockStore()
	cfg := newRuntimeSandboxConfig(t)
	mgr := NewSingleRealmProvider(store, &PlatformDeps{}, cfg, time.Minute)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]*Realm, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = mgr.Get(context.Background())
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := results[0]
	for i := 1; i < n; i++ {
		if results[i] != first {
			t.Fatalf("runtime %d does not match first instance", i)
		}
	}
}
