package kanban

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Helpers — schedulers are typically wired through Kanban.New(WithScheduler).
// Provide a single helper so individual tests stop building boilerplate.
// ---------------------------------------------------------------------------

func newScheduler(t *testing.T, opts ...Option) (*Scheduler, *Kanban, *Board) {
	t.Helper()
	sched := NewScheduler()
	allOpts := append([]Option{WithScheduler(sched), WithConfig(KanbanConfig{MaxPendingTasks: 100})}, opts...)
	k, b := newKanban(t, allOpts...)
	return sched, k, b
}

func boolPtr(v bool) *bool { return &v }

// ---------------------------------------------------------------------------
// SyncAgent / RemoveAgent — static schedule registration
// ---------------------------------------------------------------------------

func TestScheduler_SyncAgent_RegistersEnabledJobs(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)

	s.SyncAgent("agent-1", []CronJob{
		{ID: "sched-1", Cron: "* * * * *", Query: "do something", Enabled: boolPtr(true)},
	})

	s.fire("agent-1", "sched-1", "do something")

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("Cards()=%d, want 1", len(cards))
	}
	if cards[0].Meta["schedule_id"] != "sched-1" {
		t.Errorf("meta schedule_id = %v, want sched-1", cards[0].Meta["schedule_id"])
	}
}

func TestScheduler_SyncAgent_SkipsDisabledOrInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		jobs []CronJob
	}{
		{"disabled", []CronJob{{ID: "x", Cron: "* * * * *", Query: "q", Enabled: boolPtr(false)}}},
		{"invalid_cron", []CronJob{{ID: "y", Cron: "not-a-cron", Query: "q", Enabled: boolPtr(true)}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, _ := newScheduler(t)
			s.SyncAgent("agent-1", tc.jobs)

			s.mu.RLock()
			defer s.mu.RUnlock()
			if got := len(s.entries["agent-1"]); got != 0 {
				t.Errorf("entries=%d, want 0", got)
			}
		})
	}
}

func TestScheduler_RemoveAgent(t *testing.T) {
	t.Parallel()
	s, _, _ := newScheduler(t)

	s.SyncAgent("agent-1", []CronJob{
		{ID: "sched-1", Cron: "* * * * *", Query: "q", Enabled: boolPtr(true)},
	})
	s.RemoveAgent("agent-1")

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := len(s.entries["agent-1"]); got != 0 {
		t.Errorf("entries after Remove=%d, want 0", got)
	}
}

func TestScheduler_SyncAgent_PreservesDynamicEntries(t *testing.T) {
	t.Parallel()
	s, _, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	s.SyncAgent("agent-1", []CronJob{
		{ID: "static-1", Cron: "0 0 1 1 *", Query: "old", Enabled: boolPtr(true), Source: "static"},
	})
	if _, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "dynamic task",
	}, "0 0 1 1 *", ""); err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}

	s.mu.RLock()
	beforeCount := len(s.entries["agent-1"])
	s.mu.RUnlock()
	if beforeCount != 2 {
		t.Fatalf("setup: entries=%d, want 2", beforeCount)
	}

	s.SyncAgent("agent-1", []CronJob{
		{ID: "static-2", Cron: "0 0 1 1 *", Query: "new", Enabled: boolPtr(true), Source: "static"},
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := s.entries["agent-1"]
	if len(entries) != 2 {
		t.Fatalf("after re-sync: entries=%d, want 2 (1 new static + 1 dynamic)", len(entries))
	}

	sources := map[string]int{}
	for _, e := range entries {
		sources[e.source]++
	}
	if sources["dynamic"] != 1 || sources["static"] != 1 {
		t.Errorf("source breakdown = %v, want 1 each", sources)
	}
}

// ---------------------------------------------------------------------------
// fire — cron tick handler. Must be idempotent and tolerant of nil Kanban.
// ---------------------------------------------------------------------------

func TestScheduler_Fire_Idempotent(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)

	s.fire("agent-1", "sched-1", "task query")
	s.fire("agent-1", "sched-1", "task query") // same schedule_id — must dedup

	if got := len(k.QueryCards(CardFilter{Type: "task"})); got != 1 {
		t.Fatalf("Cards()=%d, want 1 (idempotent fire)", got)
	}
}

func TestScheduler_Fire_NilKanbanDoesNotPanic(t *testing.T) {
	t.Parallel()
	s := NewScheduler()
	t.Cleanup(s.Stop)
	s.fire("agent-1", "sched-1", "test") // must not panic
}

// Bug 3 (P2): scheduler-fired cards must be tagged Producer="scheduler".
func TestScheduler_Fire_TagsProducerAsScheduler(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.fire("agent-1", "sched-1", "cron task")

	cards := k.QueryCards(CardFilter{Type: "task"})
	if len(cards) != 1 {
		t.Fatalf("Cards()=%d, want 1", len(cards))
	}
	if got := cards[0].Producer; got != "scheduler" {
		t.Fatalf("Producer=%q, want scheduler", got)
	}
}

// Bug 3 (P2): fire must respect Kanban's MaxPendingTasks rate limit.
func TestScheduler_Fire_RespectsMaxPendingTasks(t *testing.T) {
	t.Parallel()
	sched := NewScheduler()
	sb := newBoard(t)
	k := New(context.Background(), sb, WithScheduler(sched), WithConfig(KanbanConfig{MaxPendingTasks: 1}))
	t.Cleanup(k.Stop)

	sched.fire("agent-1", "sched-A", "task1")
	sched.fire("agent-2", "sched-B", "task2") // different schedule_id avoids dedup

	if got := len(k.QueryCards(CardFilter{Type: "task", Status: CardPending})); got > 1 {
		t.Fatalf("MaxPendingTasks=1 not enforced for fire: %d pending", got)
	}
}

// ---------------------------------------------------------------------------
// submitWithDelay — schedules a one-shot task after a duration.
// ---------------------------------------------------------------------------

func TestScheduler_SubmitWithDelay_FiresAfterDelay(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	id, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "delayed task",
	}, "50ms")
	if err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty placeholder ID")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(k.QueryCards(CardFilter{Type: "task"})) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delayed task did not appear within deadline")
}

func TestScheduler_SubmitWithDelay_RejectsInvalidDuration(t *testing.T) {
	t.Parallel()
	s, _, _ := newScheduler(t)

	_, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "bad",
	}, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid delay")
	}
}

func TestScheduler_SubmitWithDelay_NilKanbanSafe(t *testing.T) {
	t.Parallel()
	s := NewScheduler()
	s.Start()
	t.Cleanup(s.Stop)

	if _, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "delayed",
	}, "10ms"); err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let timer fire against nil Kanban
}

// Bug 3 (P2): submitWithDelay must preserve ProducerID across the delay.
func TestScheduler_SubmitWithDelay_PreservesProducerID(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	ctx := WithProducerID(context.Background(), "user-42")
	if _, err := s.submitWithDelay(ctx, TaskOptions{
		TargetAgentID: "agent-1", Query: "delayed",
	}, "30ms"); err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cards := k.QueryCards(CardFilter{Type: "task"})
		if len(cards) == 1 {
			if cards[0].Producer != "user-42" {
				t.Fatalf("delayed task lost producer ID: got %q, want user-42", cards[0].Producer)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delayed task did not appear within deadline")
}

func TestScheduler_StopCancelsDelayTimer(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.Start()

	if _, err := s.submitWithDelay(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "should not fire",
	}, "1s"); err != nil {
		t.Fatalf("submitWithDelay: %v", err)
	}

	s.Stop()
	time.Sleep(1500 * time.Millisecond)

	if got := len(k.QueryCards(CardFilter{Type: "task"})); got != 0 {
		t.Fatalf("Cards()=%d after Stop, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// submitWithCron — dynamic cron rule + cron_rule card persistence
// ---------------------------------------------------------------------------

func TestScheduler_SubmitWithCron_RegistersDynamicEntry(t *testing.T) {
	t.Parallel()
	s, _, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	id, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "cron task",
	}, "0 0 1 1 *", "")
	if err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty schedule ID")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := s.entries["agent-1"]
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	if entries[0].source != "dynamic" {
		t.Errorf("source=%q, want dynamic", entries[0].source)
	}
}

func TestScheduler_SubmitWithCron_ProducesCronRuleCard(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	id, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "daily report",
	}, "0 9 * * *", "Asia/Shanghai")
	if err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}

	cards := k.QueryCards(CardFilter{Type: cardTypeCronRule})
	if len(cards) != 1 {
		t.Fatalf("cron_rule cards=%d, want 1", len(cards))
	}
	c := cards[0]
	if c.Status != CardPending || c.Meta["schedule_id"] != id || c.Meta["agent_id"] != "agent-1" {
		t.Errorf("unexpected card: status=%s meta=%v", c.Status, c.Meta)
	}

	p, ok := parseCronRulePayload(c.Payload)
	if !ok {
		t.Fatal("parseCronRulePayload: ok=false")
	}
	if p.AgentID != "agent-1" || p.Cron != "0 9 * * *" || p.Timezone != "Asia/Shanghai" || p.Query != "daily report" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestScheduler_SubmitWithCron_TimezoneAccepted(t *testing.T) {
	t.Parallel()
	s, _, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	id, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "tz task",
	}, "0 9 * * MON-FRI", "Asia/Shanghai")
	if err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty schedule ID")
	}
}

func TestScheduler_SubmitWithCron_MultipleEntriesAllPersisted(t *testing.T) {
	t.Parallel()
	s, k, _ := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	for i := 0; i < 3; i++ {
		if _, err := s.submitWithCron(context.Background(), TaskOptions{
			TargetAgentID: "agent-1", Query: "task",
		}, "0 * * * *", ""); err != nil {
			t.Fatalf("submitWithCron[%d]: %v", i, err)
		}
	}

	cards := k.QueryCards(CardFilter{Type: cardTypeCronRule})
	if len(cards) != 3 {
		t.Fatalf("cron_rule cards=%d, want 3", len(cards))
	}
	ids := make(map[string]bool)
	for _, c := range cards {
		ids[c.Meta["schedule_id"]] = true
	}
	if len(ids) != 3 {
		t.Fatalf("schedule_id uniqueness lost: %d unique", len(ids))
	}
}

// ---------------------------------------------------------------------------
// LoadFromBoard — restore dynamic schedules from persisted cron_rule cards
// ---------------------------------------------------------------------------

func TestScheduler_LoadFromBoard_LoadsPendingRules(t *testing.T) {
	t.Parallel()
	s, _, sb := newScheduler(t)

	sb.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID: "agent-1", ScheduleID: "dyn-1",
		Cron: "0 9 * * *", Query: "morning report", Timezone: "Asia/Shanghai",
	}, WithMeta("schedule_id", "dyn-1"), WithMeta("agent_id", "agent-1"))

	sb.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID: "agent-2", ScheduleID: "dyn-2",
		Cron: "0 18 * * *", Query: "evening summary",
	}, WithMeta("schedule_id", "dyn-2"), WithMeta("agent_id", "agent-2"))

	if n := s.LoadFromBoard(); n != 2 {
		t.Fatalf("LoadFromBoard()=%d, want 2", n)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.entries["agent-1"]; len(got) != 1 || got[0].scheduleID != "dyn-1" {
		t.Errorf("agent-1: %+v", got)
	}
	if got := s.entries["agent-2"]; len(got) != 1 || got[0].scheduleID != "dyn-2" {
		t.Errorf("agent-2: %+v", got)
	}
}

func TestScheduler_LoadFromBoard_SkipsDoneRules(t *testing.T) {
	t.Parallel()
	s, _, sb := newScheduler(t)

	c := sb.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
		AgentID: "agent-1", ScheduleID: "dyn-done",
		Cron: "0 9 * * *", Query: "done task",
	}, WithMeta("schedule_id", "dyn-done"), WithMeta("agent_id", "agent-1"))
	sb.Claim(c.ID, "system")
	sb.Done(c.ID, nil)

	if n := s.LoadFromBoard(); n != 0 {
		t.Fatalf("LoadFromBoard()=%d, want 0 (done card skipped)", n)
	}
}

// ---------------------------------------------------------------------------
// Auto-wiring (K-7): WithScheduler must register Kanban back-reference.
// ---------------------------------------------------------------------------

func TestScheduler_AutoWiredByKanbanNew(t *testing.T) {
	t.Parallel()
	sched, k, _ := newScheduler(t)
	if sched.kanban != k {
		t.Fatal("scheduler.kanban not auto-wired by New()")
	}
}

func TestScheduler_AutoWired_CanFire(t *testing.T) {
	t.Parallel()
	sched, k, _ := newScheduler(t)
	sched.fire("agent-1", "sched-1", "auto-wired task")

	if got := len(k.QueryCards(CardFilter{Type: "task"})); got != 1 {
		t.Fatalf("Cards()=%d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// CronJob.isEnabled / parseCronRulePayload — table-driven leaf helpers
// ---------------------------------------------------------------------------

func TestCronJob_IsEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		job  CronJob
		want bool
	}{
		{"nil_means_enabled", CronJob{Enabled: nil}, true},
		{"true", CronJob{Enabled: boolPtr(true)}, true},
		{"false", CronJob{Enabled: boolPtr(false)}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.job.isEnabled(); got != tc.want {
				t.Errorf("isEnabled()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseCronRulePayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    any
		ok    bool
		check func(t *testing.T, p cronRulePayload)
	}{
		{
			name: "valid_map",
			in: map[string]any{
				"agent_id": "a1", "schedule_id": "s1",
				"cron": "0 9 * * *", "query": "q", "timezone": "UTC",
			},
			ok: true,
			check: func(t *testing.T, p cronRulePayload) {
				if p.AgentID != "a1" || p.ScheduleID != "s1" || p.Cron != "0 9 * * *" ||
					p.Query != "q" || p.Timezone != "UTC" {
					t.Errorf("unexpected: %+v", p)
				}
			},
		},
		{name: "wrong_type", in: "not a map", ok: false},
		{name: "missing_keys", in: map[string]any{"foo": "bar"}, ok: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, ok := parseCronRulePayload(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v", ok, tc.ok)
			}
			if ok && tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bug 6 (P3): scheduler emits cron-rule lifecycle events on the Board's bus.
// ---------------------------------------------------------------------------

func TestScheduler_Bus_PublishesCronRuleCreatedAndDisabled(t *testing.T) {
	t.Parallel()
	s, _, sb := newScheduler(t)
	s.Start()
	t.Cleanup(s.Stop)

	sub := subscribeBus(t, sb)

	id, err := s.submitWithCron(context.Background(), TaskOptions{
		TargetAgentID: "agent-1", Query: "daily",
	}, "0 9 * * *", "")
	if err != nil {
		t.Fatalf("submitWithCron: %v", err)
	}

	createdSeen := false
	for _, ev := range drainEvents(sub, 500*time.Millisecond, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventCronRuleCreated
	}) {
		if kindOf(ev) != EventCronRuleCreated {
			continue
		}
		var p CronRuleCreatedPayload
		if err := ev.Decode(&p); err != nil || p.ScheduleID != id || p.AgentID != "agent-1" {
			t.Fatalf("CronRuleCreated payload mismatch: err=%v p=%+v", err, p)
		}
		createdSeen = true
	}
	if !createdSeen {
		t.Fatal("EventCronRuleCreated not observed")
	}

	s.RemoveAgent("agent-1")

	disabledSeen := false
	for _, ev := range drainEvents(sub, 500*time.Millisecond, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventCronRuleDisabled
	}) {
		if kindOf(ev) != EventCronRuleDisabled {
			continue
		}
		var p CronRuleDisabledPayload
		if err := ev.Decode(&p); err != nil || p.AgentID != "agent-1" {
			t.Fatalf("CronRuleDisabled payload mismatch: err=%v p=%+v", err, p)
		}
		disabledSeen = true
	}
	if !disabledSeen {
		t.Fatal("EventCronRuleDisabled not observed")
	}
}

func TestScheduler_Bus_PublishesCronRuleFiredOnFire(t *testing.T) {
	t.Parallel()
	s, _, sb := newScheduler(t)
	sub := subscribeBus(t, sb)

	s.fire("agent-1", "sched-fire", "cron task")

	for _, ev := range drainEvents(sub, 500*time.Millisecond, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventCronRuleFired
	}) {
		if kindOf(ev) != EventCronRuleFired {
			continue
		}
		var p CronRuleFiredPayload
		if err := ev.Decode(&p); err != nil || p.ScheduleID != "sched-fire" || p.AgentID != "agent-1" {
			t.Fatalf("CronRuleFired payload mismatch: err=%v p=%+v", err, p)
		}
		return
	}
	t.Fatal("EventCronRuleFired not observed")
}
