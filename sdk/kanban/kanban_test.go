package kanban

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type mockExecutor struct {
	fn func(ctx context.Context, scopeID, targetAgentID string, card *Card, query string, inputs map[string]any) error
}

func (m *mockExecutor) ExecuteTask(ctx context.Context, scopeID, targetAgentID string, card *Card, query string, inputs map[string]any) error {
	if m.fn != nil {
		return m.fn(ctx, scopeID, targetAgentID, card, query, inputs)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

func TestKanban_Submit(t *testing.T) {
	sb := NewBoard("scope-1")
	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	defer k.Stop()

	ctx := context.Background()
	cardID, err := k.Submit(ctx, TaskOptions{
		TargetAgentID: "copilot_builder",
		Query:         "create RAG app",
		UserQuery:     "帮我创建一个 RAG 应用",
		DispatchNote:  "完成后告知用户",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if cardID == "" {
		t.Fatal("expected non-empty card ID")
	}

	card, err := k.GetCard(ctx, cardID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	p := PayloadMap(card.Payload)
	if p["user_query"] != "帮我创建一个 RAG 应用" {
		t.Fatalf("expected user_query in payload, got %v", p["user_query"])
	}
	if p["dispatch_note"] != "完成后告知用户" {
		t.Fatalf("expected dispatch_note in payload, got %v", p["dispatch_note"])
	}
	if p["target_agent_id"] != "copilot_builder" {
		t.Fatalf("expected target_agent_id in payload, got %v", p["target_agent_id"])
	}
}

func TestKanban_Submit_PendingLimit(t *testing.T) {
	sb := NewBoard("scope-2")
	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 2}))

	ctx := context.Background()

	_, err := k.Submit(ctx, TaskOptions{Query: "task 1"})
	if err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	_, err = k.Submit(ctx, TaskOptions{Query: "task 2"})
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}

	_, err = k.Submit(ctx, TaskOptions{Query: "task 3"})
	if err == nil {
		t.Fatal("expected pending limit error")
	}
}

func TestKanban_Submit_StructuredErrors(t *testing.T) {
	sb := NewBoard("scope-err")

	k := New(context.Background(), sb, WithConfig(KanbanConfig{MaxPendingTasks: 1}))
	ctx := context.Background()
	_, _ = k.Submit(ctx, TaskOptions{Query: "task 1"})
	_, err := k.Submit(ctx, TaskOptions{Query: "task 2"})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("expected RateLimit error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetCard / QueryCards
// ---------------------------------------------------------------------------

func TestKanban_GetCard(t *testing.T) {
	sb := NewBoard("scope-gc")
	k := New(context.Background(), sb)

	card := sb.Produce("task", "orch", "payload1")
	got, err := k.GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.ID != card.ID {
		t.Fatalf("expected %q, got %q", card.ID, got.ID)
	}

	_, err = k.GetCard(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent card")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound error, got %v", err)
	}
}

func TestKanban_QueryCards(t *testing.T) {
	sb := NewBoard("scope-qc")
	k := New(context.Background(), sb)

	sb.Produce("task", "orch", "payload1")
	sb.Produce("task", "orch", "payload2")
	sb.Produce("signal", "orch", "stop")

	tasks := k.QueryCards(CardFilter{Type: "task"})
	if len(tasks) != 2 {
		t.Fatalf("expected 2 task cards, got %d", len(tasks))
	}

	signals := k.QueryCards(CardFilter{Type: "signal"})
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal card, got %d", len(signals))
	}

	all := k.QueryCards(CardFilter{})
	if len(all) != 3 {
		t.Fatalf("expected 3 total cards, got %d", len(all))
	}
}

// ---------------------------------------------------------------------------
// Broadcast
// ---------------------------------------------------------------------------

func TestKanban_Broadcast(t *testing.T) {
	sb := NewBoard("scope-4")
	k := New(context.Background(), sb)

	ctx := context.Background()
	k.Broadcast(ctx, "stop_all", map[string]string{"reason": "test"})

	cards := sb.Query(CardFilter{Type: "signal"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 signal card, got %d", len(cards))
	}
}

// ---------------------------------------------------------------------------
// Executor — success
// ---------------------------------------------------------------------------

func TestKanban_Submit_WithExecutor(t *testing.T) {
	sb := NewBoard("scope-exec")
	executed := make(chan struct{})
	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			close(executed)
			return nil
		},
	}

	k := New(context.Background(), sb, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	defer k.Stop()

	_, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "builder",
		Query:         "build something",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		t.Fatal("executor was not called")
	}
}

func TestKanban_ExecutorClaimAndDone(t *testing.T) {
	sb := NewBoard("scope-ev")
	defer sb.Close()

	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			sb.Claim(card.ID, targetAgentID)
			sb.Done(card.ID, map[string]any{
				"output":          "agent output",
				"target_agent_id": targetAgentID,
			})
			return nil
		},
	}

	k := New(context.Background(), sb,
		WithAgentExecutor(executor),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
		WithEventBus(sb.Bus()),
	)

	ctx := context.Background()
	sub, err := sb.Bus().Subscribe(ctx, event.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()

	_, err = k.Submit(ctx, TaskOptions{
		TargetAgentID: "test-agent",
		Query:         "hello agent",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	var received []event.Event
	drainTimeout := time.After(time.Second)
drain:
	for {
		select {
		case ev := <-sub.Events():
			received = append(received, ev)
		case <-drainTimeout:
			break drain
		}
	}

	hasSubmitted := false
	for _, ev := range received {
		if ev.Type == EventTaskSubmitted {
			hasSubmitted = true
		}
	}
	if !hasSubmitted {
		t.Fatal("expected task.submitted event on Bus")
	}
}

// ---------------------------------------------------------------------------
// Executor — failure (K-1)
// ---------------------------------------------------------------------------

func TestKanban_ExecutorError_MarksCardFailed(t *testing.T) {
	sb := NewBoard("scope-k1-fail")
	defer sb.Close()

	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			return fmt.Errorf("simulated executor crash")
		},
	}

	k := New(context.Background(), sb,
		WithAgentExecutor(executor),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)

	cardID, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "agent-x",
		Query:         "do work",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	card, err := k.GetCard(context.Background(), cardID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if card.Status != CardFailed {
		t.Fatalf("expected card status Failed, got %s", card.Status)
	}
	if card.Error != "simulated executor crash" {
		t.Fatalf("expected error message on card, got %q", card.Error)
	}
}

func TestKanban_ExecutorError_PublishesTaskFailedEvent(t *testing.T) {
	sb := NewBoard("scope-k1-ev")
	defer sb.Close()

	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			return fmt.Errorf("agent down")
		},
	}

	k := New(context.Background(), sb,
		WithAgentExecutor(executor),
		WithEventBus(bus),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)

	sub, err := bus.Subscribe(context.Background(), event.EventFilter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	_, err = k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "agent-fail",
		Query:         "boom",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	var hasFailed bool
	timeout := time.After(2 * time.Second)
drainFailed:
	for {
		select {
		case ev := <-sub.Events():
			if string(ev.Type) == EventTaskFailed {
				hasFailed = true
				p, ok := ev.Payload.(TaskFailedPayload)
				if !ok {
					break drainFailed
				}
				if p.Error != "agent down" {
					t.Fatalf("expected error='agent down', got %q", p.Error)
				}
				if p.TargetAgentID != "agent-fail" {
					t.Fatalf("expected target_agent_id='agent-fail', got %q", p.TargetAgentID)
				}
				break drainFailed
			}
		case <-timeout:
			break drainFailed
		}
	}
	if !hasFailed {
		t.Fatal("expected EventTaskFailed to be published when executor returns error")
	}
}

func TestKanban_ExecutorCallsFailThenReturnsError(t *testing.T) {
	sb := NewBoard("scope-fail-both")
	defer sb.Close()

	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			sb.Claim(card.ID, targetAgentID)
			sb.Fail(card.ID, "execution failed")
			return fmt.Errorf("execution failed")
		},
	}

	k := New(context.Background(), sb,
		WithAgentExecutor(executor),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)

	_, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "fail-agent",
		Query:         "boom",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	failed := sb.Query(CardFilter{Type: "task", Status: CardFailed})
	if len(failed) == 0 {
		t.Fatal("expected failed card after executor error")
	}
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

func TestKanban_Submit_AgentValidatorRejects(t *testing.T) {
	tb := NewBoard("scope-validate")
	validAgents := map[string]bool{"copilot-builder": true, "copilot-runner": true}
	validator := func(_ context.Context, agentID string) error {
		if validAgents[agentID] {
			return nil
		}
		return fmt.Errorf("agent %q not found; available agent IDs: [copilot-builder copilot-runner]", agentID)
	}
	k := New(context.Background(), tb,
		WithAgentValidator(validator),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)
	defer k.Stop()

	_, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "agent-builder",
		Query:         "build something",
	})
	if err == nil {
		t.Fatal("expected validation error for unknown agent")
	}
	if !strings.Contains(err.Error(), "agent-builder") {
		t.Fatalf("error should mention invalid ID, got: %v", err)
	}
	if !strings.Contains(err.Error(), "copilot-builder") {
		t.Fatalf("error should list available IDs, got: %v", err)
	}

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 0 {
		t.Fatalf("expected 0 cards after rejected submit, got %d", len(cards))
	}

	cardID, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "copilot-builder",
		Query:         "build something",
	})
	if err != nil {
		t.Fatalf("expected valid agent to pass validation: %v", err)
	}
	if cardID == "" {
		t.Fatal("expected non-empty card ID for valid agent")
	}
}

// ---------------------------------------------------------------------------
// Stop / lifecycle
// ---------------------------------------------------------------------------

func TestKanban_StopRejectsNewSubmit(t *testing.T) {
	tb := NewBoard("scope-stop")
	k := New(context.Background(), tb)

	k.Stop()
	_, err := k.Submit(context.Background(), TaskOptions{Query: "after-stop"})
	if err == nil {
		t.Fatal("expected submit to fail after stop")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable error, got %v", err)
	}
}

func TestKanban_StopWaitsInflightExecutor(t *testing.T) {
	tb := NewBoard("scope-wait")
	started := make(chan struct{})
	release := make(chan struct{})
	k := New(context.Background(), tb, WithAgentExecutor(&mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			close(started)
			<-release
			return nil
		},
	}))

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "builder",
		Query:         "build something",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	stopped := make(chan struct{})
	go func() {
		k.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop should wait for inflight executor")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after inflight executor finished")
	}
}

func TestKanban_StopCancelsExecutorContext(t *testing.T) {
	tb := NewBoard("scope-cancel")
	ctxCancelled := make(chan struct{})
	k := New(context.Background(), tb, WithAgentExecutor(&mockExecutor{
		fn: func(ctx context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			<-ctx.Done()
			close(ctxCancelled)
			return ctx.Err()
		},
	}))

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "builder",
		Query:         "build something",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	k.Stop()

	select {
	case <-ctxCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("executor context was not cancelled by Stop")
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func TestKanban_EventBusPublish(t *testing.T) {
	sb := NewBoard("scope-ev-pub")
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	k := New(context.Background(), sb, WithEventBus(bus), WithConfig(KanbanConfig{MaxPendingTasks: 100}))

	ctx := context.Background()
	sub, err := bus.Subscribe(ctx, event.EventFilter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	_, _ = k.Submit(ctx, TaskOptions{Query: "test event"})

	select {
	case ev := <-sub.Events():
		if string(ev.Type) != EventTaskSubmitted {
			t.Fatalf("expected %q, got %q", EventTaskSubmitted, ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestKanban_SubmitConcurrent(t *testing.T) {
	sb := NewBoard("scope-conc")
	defer sb.Close()

	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			time.Sleep(10 * time.Millisecond)
			sb.Claim(card.ID, targetAgentID)
			sb.Done(card.ID, map[string]any{"output": "done"})
			return nil
		},
	}

	k := New(context.Background(), sb, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := k.Submit(context.Background(), TaskOptions{
				TargetAgentID: "tpl",
				Query:         "concurrent-task",
			})
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Submit error: %v", err)
	}

	k.Stop()
}

func TestKanban_SubmitConcurrentWithStop(t *testing.T) {
	tb := NewBoard("scope-race")
	k := New(context.Background(), tb, WithAgentExecutor(&mockExecutor{
		fn: func(ctx context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
				return nil
			}
		},
	}))

	const n = 16
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, err := k.Submit(context.Background(), TaskOptions{
				TargetAgentID: "builder",
				Query:         "build something",
			})
			if err != nil && !errdefs.IsNotAvailable(err) {
				errs <- err
			}
		}()
	}
	close(start)
	time.Sleep(10 * time.Millisecond)
	k.Stop()
	close(errs)

	for err := range errs {
		t.Fatalf("unexpected submit error: %v", err)
	}
}
