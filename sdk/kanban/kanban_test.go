package kanban

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Submit — happy path & payload shaping
// ---------------------------------------------------------------------------

func TestKanban_Submit_HappyPath(t *testing.T) {
	t.Parallel()
	k, _ := newKanban(t, WithConfig(KanbanConfig{MaxPendingTasks: 100}))

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
	for key, want := range map[string]string{
		"user_query":      "帮我创建一个 RAG 应用",
		"dispatch_note":   "完成后告知用户",
		"target_agent_id": "copilot_builder",
	} {
		if p[key] != want {
			t.Fatalf("payload[%q] = %v, want %q", key, p[key], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Submit — guardrails (rate limit, validator, structured errors)
// ---------------------------------------------------------------------------

func TestKanban_Submit_PendingLimit(t *testing.T) {
	t.Parallel()
	k, _ := newKanban(t, WithConfig(KanbanConfig{MaxPendingTasks: 2}))

	for i, query := range []string{"task 1", "task 2"} {
		if _, err := k.Submit(context.Background(), TaskOptions{Query: query}); err != nil {
			t.Fatalf("submit %d: %v", i+1, err)
		}
	}

	_, err := k.Submit(context.Background(), TaskOptions{Query: "task 3"})
	if err == nil {
		t.Fatal("expected pending-limit error on 3rd submit")
	}
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("expected RateLimit error, got %v", err)
	}
}

func TestKanban_Submit_AgentValidatorRejects(t *testing.T) {
	t.Parallel()
	validAgents := map[string]bool{"copilot-builder": true, "copilot-runner": true}
	validator := func(_ context.Context, agentID string) error {
		if validAgents[agentID] {
			return nil
		}
		return fmt.Errorf("agent %q not found; available agent IDs: [copilot-builder copilot-runner]", agentID)
	}
	k, _ := newKanban(t,
		WithAgentValidator(validator),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)

	_, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "agent-builder",
		Query:         "build something",
	})
	if err == nil {
		t.Fatal("expected validation error for unknown agent")
	}
	if !strings.Contains(err.Error(), "agent-builder") || !strings.Contains(err.Error(), "copilot-builder") {
		t.Fatalf("error should name the bad ID and list valid IDs, got: %v", err)
	}

	if cards := k.QueryCards(CardFilter{Type: "task"}); len(cards) != 0 {
		t.Fatalf("expected 0 cards after rejected submit, got %d", len(cards))
	}

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "copilot-builder",
		Query:         "build something",
	}); err != nil {
		t.Fatalf("expected valid agent to pass validation: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetCard / QueryCards / Broadcast
// ---------------------------------------------------------------------------

func TestKanban_GetCard(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t)
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

func TestKanban_QueryCards_FiltersByType(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t)
	sb.Produce("task", "orch", "payload1")
	sb.Produce("task", "orch", "payload2")
	sb.Produce("signal", "orch", "stop")

	cases := []struct {
		filter CardFilter
		want   int
		label  string
	}{
		{CardFilter{Type: "task"}, 2, "task filter"},
		{CardFilter{Type: "signal"}, 1, "signal filter"},
		{CardFilter{}, 3, "no filter"},
	}
	for _, c := range cases {
		if got := len(k.QueryCards(c.filter)); got != c.want {
			t.Errorf("%s: got %d, want %d", c.label, got, c.want)
		}
	}
}

// TestKanban_Board_ReturnsUnderlyingBoard guards the Board() accessor.
// Many callers (audit, persistence layers) rely on this.
func TestKanban_Board_ReturnsUnderlyingBoard(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t)
	if k.Board() != sb {
		t.Fatal("Kanban.Board() should return the same Board passed to New()")
	}
}

// TestKanban_WithEventBus_DeprecatedNoOp pins the v0.1.x contract:
// WithEventBus is accepted for source compatibility but is a complete
// no-op. Events must continue to land on board.Bus() — the injected bus
// must remain silent. Will be removed together with WithEventBus in v0.2.0.
func TestKanban_WithEventBus_DeprecatedNoOp(t *testing.T) {
	t.Parallel()
	external := event.NewMemoryBus()
	t.Cleanup(func() { _ = external.Close() })

	extSub, err := external.Subscribe(context.Background(), event.EventFilter{})
	if err != nil {
		t.Fatalf("external.Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = extSub.Close() })

	k, sb := newKanban(t, WithEventBus(external))
	if k.Bus() != sb.Bus() {
		t.Fatal("Kanban.Bus() must alias board.Bus(); WithEventBus must not redirect it")
	}

	boardSub := subscribeBus(t, sb)

	if _, err := k.Submit(context.Background(), TaskOptions{Query: "still works"}); err != nil {
		t.Fatalf("Submit after WithEventBus: %v", err)
	}

	if got := drainEvents(boardSub, 200*time.Millisecond, 1, func(e event.Event) bool {
		return string(e.Type) == EventTaskSubmitted
	}); !containsType(got, EventTaskSubmitted) {
		t.Fatalf("expected EventTaskSubmitted on board.Bus(), got types=%v", eventTypes(got))
	}

	if got := drainEvents(extSub, 100*time.Millisecond, 0, nil); len(got) != 0 {
		t.Fatalf("WithEventBus must be a no-op; injected bus received %d events: %v",
			len(got), eventTypes(got))
	}
}

func TestKanban_Broadcast_ProducesSignalCard(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t)
	k.Broadcast(context.Background(), "stop_all", map[string]string{"reason": "test"})

	if cards := sb.Query(CardFilter{Type: "signal"}); len(cards) != 1 {
		t.Fatalf("expected 1 signal card, got %d", len(cards))
	}
}

// ---------------------------------------------------------------------------
// Executor — invocation, success, failure
// ---------------------------------------------------------------------------

func TestKanban_Submit_InvokesExecutor(t *testing.T) {
	t.Parallel()
	executed := make(chan struct{})
	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			close(executed)
			return nil
		},
	}

	k, _ := newKanban(t, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "builder",
		Query:         "build something",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		t.Fatal("executor was not invoked")
	}
}

func TestKanban_Executor_FailureMarksCardFailed(t *testing.T) {
	t.Parallel()
	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			return errors.New("simulated executor crash")
		},
	}

	k, _ := newKanban(t, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
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
		t.Fatalf("expected CardFailed, got %s", card.Status)
	}
	if card.Error != "simulated executor crash" {
		t.Fatalf("expected error message on card, got %q", card.Error)
	}
}

// TestKanban_Executor_FailIsIdempotent guards the legacy contract that an
// executor is allowed to call Fail itself and also return a non-nil error;
// only one CardFailed transition must land on the board.
func TestKanban_Executor_FailIsIdempotent(t *testing.T) {
	t.Parallel()
	sb := newBoard(t)
	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			sb.Claim(card.ID, targetAgentID)
			sb.Fail(card.ID, "execution failed")
			return errors.New("execution failed")
		},
	}
	k := New(context.Background(), sb, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	t.Cleanup(k.Stop)

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "fail-agent",
		Query:         "boom",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	k.Stop()

	if got := len(sb.Query(CardFilter{Type: "task", Status: CardFailed})); got != 1 {
		t.Fatalf("expected exactly 1 failed card, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Stop / lifecycle
// ---------------------------------------------------------------------------

func TestKanban_Stop_RejectsNewSubmits(t *testing.T) {
	t.Parallel()
	k, _ := newKanban(t)
	k.Stop()

	_, err := k.Submit(context.Background(), TaskOptions{Query: "after-stop"})
	if err == nil {
		t.Fatal("expected submit to fail after Stop")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable error, got %v", err)
	}
}

func TestKanban_Stop_WaitsForInflightExecutor(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})

	k, _ := newKanban(t, WithAgentExecutor(&mockExecutor{
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
		t.Fatal("Stop returned before inflight executor finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after inflight executor finished")
	}
}

func TestKanban_Stop_CancelsExecutorContext(t *testing.T) {
	t.Parallel()
	cancelled := make(chan struct{})
	k, _ := newKanban(t, WithAgentExecutor(&mockExecutor{
		fn: func(ctx context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			<-ctx.Done()
			close(cancelled)
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
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("executor context was not cancelled by Stop")
	}
}

// Default stopTimeout is DefaultStopTimeout (10s), not "wait forever".
// A fresh Kanban with no Option must therefore have a bounded Stop.
func TestKanban_Stop_DefaultTimeoutIsTenSeconds(t *testing.T) {
	t.Parallel()
	k, _ := newKanban(t)
	if got, want := k.stopTimeout, DefaultStopTimeout; got != want {
		t.Fatalf("default stopTimeout = %v, want %v", got, want)
	}
	if DefaultStopTimeout != 10*time.Second {
		t.Fatalf("DefaultStopTimeout = %v, want 10s", DefaultStopTimeout)
	}
}

// WithStopTimeout(0) opts back into the legacy unbounded-wait behaviour.
func TestKanban_Stop_ZeroTimeoutWaitsForever(t *testing.T) {
	t.Parallel()
	executorEntered := make(chan struct{})
	releaseExecutor := make(chan struct{})

	k, _ := newKanban(t,
		WithAgentExecutor(&mockExecutor{
			fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
				close(executorEntered)
				<-releaseExecutor
				return nil
			},
		}),
		WithStopTimeout(0),
	)
	if k.stopTimeout != 0 {
		t.Fatalf("WithStopTimeout(0) did not set stopTimeout to 0, got %v", k.stopTimeout)
	}

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "builder",
		Query:         "build something",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-executorEntered

	stopped := make(chan struct{})
	go func() {
		k.Stop()
		close(stopped)
	}()

	// With zero timeout, Stop must NOT return while the executor is still
	// running, even after a window much larger than DefaultStopTimeout would
	// have allowed if it had silently leaked through.
	select {
	case <-stopped:
		t.Fatal("Stop with WithStopTimeout(0) returned before executor finished")
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseExecutor)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after executor finished")
	}
}

// Bug 4 (P2): Stop must respect WithStopTimeout and not hang on stuck executors.
func TestKanban_Stop_RespectsStopTimeout(t *testing.T) {
	t.Parallel()
	executorEntered := make(chan struct{})
	releaseExecutor := make(chan struct{})
	defer close(releaseExecutor)

	k, _ := newKanban(t,
		WithAgentExecutor(&mockExecutor{
			fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
				close(executorEntered)
				<-releaseExecutor
				return nil
			},
		}),
		WithStopTimeout(100*time.Millisecond),
	)

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "stuck",
		Query:         "ignore cancel",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-executorEntered

	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		k.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop with WithStopTimeout did not return within budget")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop returned but took %v, expected ~100ms", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Concurrency — Submit racing under load and against Stop
// ---------------------------------------------------------------------------

func TestKanban_Submit_Concurrent(t *testing.T) {
	t.Parallel()
	sb := newBoard(t)
	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			time.Sleep(10 * time.Millisecond)
			sb.Claim(card.ID, targetAgentID)
			sb.Done(card.ID, map[string]any{"output": "done"})
			return nil
		},
	}
	k := New(context.Background(), sb, WithAgentExecutor(executor), WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	t.Cleanup(k.Stop)

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := k.Submit(context.Background(), TaskOptions{
				TargetAgentID: "tpl",
				Query:         "concurrent-task",
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Submit error: %v", err)
	}
}

func TestKanban_Submit_RacesWithStop(t *testing.T) {
	t.Parallel()
	k, _ := newKanban(t, WithAgentExecutor(&mockExecutor{
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
		t.Fatalf("unexpected submit error during Submit/Stop race: %v", err)
	}
}
