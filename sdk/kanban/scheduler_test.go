package kanban

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestKanbanForScheduler(t *testing.T) *Kanban {
	t.Helper()
	board := NewBoard("test-scheduler")
	return New(context.Background(), board)
}

func TestScheduler_SyncAndFire(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	enabled := true
	s.SyncAgent("agent-1", []CronJob{
		{ID: "sched-1", Cron: "* * * * *", Query: "do something", Enabled: &enabled},
	})

	s.fire("agent-1", "sched-1", "do something")

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].Meta["schedule_id"] != "sched-1" {
		t.Errorf("expected meta schedule_id=sched-1, got %v", cards[0].Meta)
	}
}

func TestScheduler_FireIdempotency(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	s.fire("agent-1", "sched-1", "task query")
	s.fire("agent-1", "sched-1", "task query")

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card (idempotency), got %d", len(cards))
	}
}

func TestScheduler_DisabledJobSkipped(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	disabled := false
	s.SyncAgent("agent-1", []CronJob{
		{ID: "sched-disabled", Cron: "* * * * *", Query: "should not run", Enabled: &disabled},
	})

	s.mu.RLock()
	entries := s.entries["agent-1"]
	s.mu.RUnlock()
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for disabled job, got %d", len(entries))
	}
}

func TestScheduler_RemoveAgent(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	enabled := true
	s.SyncAgent("agent-1", []CronJob{
		{ID: "sched-1", Cron: "* * * * *", Query: "q", Enabled: &enabled},
	})
	s.RemoveAgent("agent-1")

	s.mu.RLock()
	entries := s.entries["agent-1"]
	s.mu.RUnlock()
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after remove, got %d", len(entries))
	}
}

func TestScheduler_SyncAgentReplacesStaticOnly(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	enabled := true
	s.SyncAgent("agent-1", []CronJob{
		{ID: "static-1", Cron: "0 0 1 1 *", Query: "old query", Enabled: &enabled, Source: "static"},
	})

	// Add a dynamic entry via submitWithCron
	_, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "dynamic task",
	}, "0 0 1 1 *", "")
	if err != nil {
		t.Fatal(err)
	}

	s.mu.RLock()
	beforeCount := len(s.entries["agent-1"])
	s.mu.RUnlock()
	if beforeCount != 2 {
		t.Fatalf("expected 2 entries (1 static + 1 dynamic), got %d", beforeCount)
	}

	// Re-sync static schedules — dynamic should survive
	s.SyncAgent("agent-1", []CronJob{
		{ID: "static-2", Cron: "0 0 1 1 *", Query: "new query", Enabled: &enabled, Source: "static"},
	})

	s.mu.RLock()
	entries := s.entries["agent-1"]
	s.mu.RUnlock()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (1 new static + 1 dynamic), got %d", len(entries))
	}

	sources := map[string]int{}
	for _, e := range entries {
		sources[e.source]++
	}
	if sources["dynamic"] != 1 {
		t.Errorf("expected 1 dynamic entry, got %d", sources["dynamic"])
	}
	if sources["static"] != 1 {
		t.Errorf("expected 1 static entry, got %d", sources["static"])
	}
}

func TestScheduler_SubmitWithDelay(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	var wg sync.WaitGroup
	wg.Add(1)

	origSubmit := k.QueryCards
	_ = origSubmit

	id, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "delayed task",
	}, "50ms")
	if err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty placeholder ID")
	}

	time.Sleep(150 * time.Millisecond)

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card after delay, got %d", len(cards))
	}
}

func TestScheduler_SubmitWithInvalidDelay(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	_, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "bad",
	}, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid delay")
	}
}

func TestScheduler_SubmitWithCron(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	schedID, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "cron task",
	}, "0 0 1 1 *", "")
	if err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}
	if schedID == "" {
		t.Fatal("expected non-empty schedule ID")
	}

	s.mu.RLock()
	entries := s.entries["agent-1"]
	s.mu.RUnlock()
	if len(entries) != 1 {
		t.Fatalf("expected 1 cron entry, got %d", len(entries))
	}
	if entries[0].source != "dynamic" {
		t.Errorf("expected source=dynamic, got %q", entries[0].source)
	}
}

func TestScheduler_SubmitWithCron_ProducesCronRuleCard(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	schedID, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "daily report",
	}, "0 9 * * *", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}

	cards := k.QueryCards(CardFilter{Type: cardTypeCronRule})
	if len(cards) != 1 {
		t.Fatalf("expected 1 cron_rule card, got %d", len(cards))
	}
	card := cards[0]
	if card.Status != CardPending {
		t.Errorf("expected status=pending, got %s", card.Status)
	}
	if card.Meta["schedule_id"] != schedID {
		t.Errorf("expected meta schedule_id=%s, got %s", schedID, card.Meta["schedule_id"])
	}
	if card.Meta["agent_id"] != "agent-1" {
		t.Errorf("expected meta agent_id=agent-1, got %s", card.Meta["agent_id"])
	}

	p, ok := parseCronRulePayload(card.Payload)
	if !ok {
		t.Fatal("failed to parse cron_rule payload")
	}
	if p.AgentID != "agent-1" {
		t.Errorf("payload agent_id = %q, want agent-1", p.AgentID)
	}
	if p.Cron != "0 9 * * *" {
		t.Errorf("payload cron = %q, want raw expression", p.Cron)
	}
	if p.Timezone != "Asia/Shanghai" {
		t.Errorf("payload timezone = %q, want Asia/Shanghai", p.Timezone)
	}
	if p.Query != "daily report" {
		t.Errorf("payload query = %q, want daily report", p.Query)
	}
}

func TestScheduler_LoadFromBoard(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	// Produce some cron_rule cards on the board (simulating restored state)
	board := k.Board()
	board.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID:    "agent-1",
		ScheduleID: "dyn-1",
		Cron:       "0 9 * * *",
		Query:      "morning report",
		Timezone:   "Asia/Shanghai",
	}, WithMeta("schedule_id", "dyn-1"), WithMeta("agent_id", "agent-1"))

	board.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID:    "agent-2",
		ScheduleID: "dyn-2",
		Cron:       "0 18 * * *",
		Query:      "evening summary",
	}, WithMeta("schedule_id", "dyn-2"), WithMeta("agent_id", "agent-2"))

	n := s.LoadFromBoard()
	if n != 2 {
		t.Fatalf("expected 2 loaded, got %d", n)
	}

	s.mu.RLock()
	agent1 := s.entries["agent-1"]
	agent2 := s.entries["agent-2"]
	s.mu.RUnlock()

	if len(agent1) != 1 || agent1[0].scheduleID != "dyn-1" {
		t.Errorf("agent-1 entries: %+v", agent1)
	}
	if len(agent2) != 1 || agent2[0].scheduleID != "dyn-2" {
		t.Errorf("agent-2 entries: %+v", agent2)
	}
}

func TestScheduler_LoadFromBoard_SkipsDoneCards(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	board := k.Board()
	card := board.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID:    "agent-1",
		ScheduleID: "dyn-done",
		Cron:       "0 9 * * *",
		Query:      "done task",
	}, WithMeta("schedule_id", "dyn-done"), WithMeta("agent_id", "agent-1"))

	board.Claim(card.ID, "system")
	board.Done(card.ID, nil)

	n := s.LoadFromBoard()
	if n != 0 {
		t.Fatalf("expected 0 loaded (done card), got %d", n)
	}
}

func TestScheduler_StopCancelsDelayTimer(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()

	_, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "should not fire",
	}, "1s")
	if err != nil {
		t.Fatal(err)
	}

	s.Stop()
	time.Sleep(1500 * time.Millisecond)

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 0 {
		t.Fatalf("expected 0 cards after stop, got %d", len(cards))
	}
}

func TestScheduler_CronJobIsEnabled(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name string
		job  CronJob
		want bool
	}{
		{"nil = enabled", CronJob{Enabled: nil}, true},
		{"true = enabled", CronJob{Enabled: &enabled}, true},
		{"false = disabled", CronJob{Enabled: &disabled}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.isEnabled(); got != tt.want {
				t.Errorf("isEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScheduler_InvalidCronExpressionSkipped(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)

	enabled := true
	s.SyncAgent("agent-1", []CronJob{
		{ID: "bad", Cron: "not-a-cron", Query: "q", Enabled: &enabled},
	})

	s.mu.RLock()
	entries := s.entries["agent-1"]
	s.mu.RUnlock()
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for invalid cron, got %d", len(entries))
	}
}

func TestScheduler_FireWithNilKanban(t *testing.T) {
	s := NewScheduler()
	s.fire("agent-1", "sched-1", "test")
}

func TestScheduler_TimezoneInCronExpr(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	schedID, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "tz task",
	}, "0 9 * * MON-FRI", "Asia/Shanghai")
	if err != nil {
		t.Fatalf("submitWithCron with timezone: %v", err)
	}
	if schedID == "" {
		t.Fatal("expected non-empty schedule ID")
	}
}

func TestScheduler_MultipleDynamicCrons_AllProduceCards(t *testing.T) {
	k := newTestKanbanForScheduler(t)
	s := NewScheduler()
	s.SetKanban(k)
	s.Start()
	defer s.Stop()

	for i := 0; i < 3; i++ {
		_, err := s.submitWithCron(context.Background(), TaskOptions{
			TargetAgentID: "agent-1",
			Query:         "task",
		}, "0 * * * *", "")
		if err != nil {
			t.Fatal(err)
		}
	}

	cards := k.QueryCards(CardFilter{Type: cardTypeCronRule})
	if len(cards) != 3 {
		t.Fatalf("expected 3 cron_rule cards, got %d", len(cards))
	}

	ids := make(map[string]bool)
	for _, c := range cards {
		ids[c.Meta["schedule_id"]] = true
	}
	if len(ids) != 3 {
		t.Fatal("expected 3 unique schedule IDs in cards")
	}
}

func TestScheduler_ParseCronRulePayload(t *testing.T) {
	valid := map[string]any{
		"agent_id":    "a1",
		"schedule_id": "s1",
		"cron":        "0 9 * * *",
		"query":       "q",
		"timezone":    "UTC",
	}
	p, ok := parseCronRulePayload(valid)
	if !ok {
		t.Fatal("expected ok=true for valid payload")
	}
	if p.AgentID != "a1" || p.ScheduleID != "s1" || p.Cron != "0 9 * * *" || p.Query != "q" || p.Timezone != "UTC" {
		t.Errorf("unexpected parsed payload: %+v", p)
	}

	_, ok = parseCronRulePayload("not a map")
	if ok {
		t.Error("expected ok=false for string payload")
	}

	_, ok = parseCronRulePayload(map[string]any{"foo": "bar"})
	if ok {
		t.Error("expected ok=false for payload missing agent_id/schedule_id")
	}
}

// ---------------------------------------------------------------------------
// WithScheduler auto-wiring (K-7)
// ---------------------------------------------------------------------------

func TestScheduler_AutoWiredByKanbanNew(t *testing.T) {
	sb := NewBoard("scope-k7")
	defer sb.Close()

	sched := NewScheduler()
	k := New(context.Background(), sb, WithScheduler(sched))
	defer k.Stop()

	if sched.kanban != k {
		t.Fatal("expected scheduler.kanban to be auto-wired by New()")
	}
}

func TestScheduler_AutoWired_CanFire(t *testing.T) {
	sb := NewBoard("scope-k7-fire")
	defer sb.Close()

	sched := NewScheduler()
	k := New(context.Background(), sb, WithScheduler(sched))
	defer k.Stop()

	sched.fire("agent-1", "sched-1", "auto-wired task")

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card after fire, got %d", len(cards))
	}
}

func TestScheduler_SubmitWithDelay_NilKanbanSafe(t *testing.T) {
	s := NewScheduler()
	s.Start()

	_, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1",
		Query:         "delayed",
	}, "10ms")
	if err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	s.Stop()
}
