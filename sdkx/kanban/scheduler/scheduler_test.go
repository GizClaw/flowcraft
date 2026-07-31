package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdkx/kanban/scheduler"
)

func newBoard(t *testing.T) *kanban.Kanban {
	t.Helper()
	k := kanban.New("sched-test")
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func newSched(t *testing.T, k *kanban.Kanban, opts ...scheduler.Option) *scheduler.Scheduler {
	t.Helper()
	s, err := scheduler.New(k, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func task(target string) kanban.Task {
	return kanban.Task{TargetAgentID: target, Query: "scheduled work"}
}

// waitForCards polls until the board holds n cards or the deadline
// passes. Polling beats a fixed sleep: it fails fast and is not flaky
// under load.
func waitForCards(t *testing.T, k *kanban.Kanban, n int) []*kanban.Card {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		cards := k.Query(kanban.Filter{})
		if len(cards) >= n {
			return cards
		}
		if time.Now().After(deadline) {
			t.Fatalf("board holds %d cards, want %d", len(cards), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNewRequiresBoard(t *testing.T) {
	if _, err := scheduler.New(nil); !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestAfterSubmitsOnceAfterDelay(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	handle, err := s.After(context.Background(), 10*time.Millisecond, task("worker"))
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if handle == "" {
		t.Fatal("After returned an empty handle")
	}
	if k.Len() != 0 {
		t.Fatal("After submitted immediately; the delay was not honoured")
	}

	cards := waitForCards(t, k, 1)
	if cards[0].Task.TargetAgentID != "worker" {
		t.Errorf("TargetAgentID = %q", cards[0].Task.TargetAgentID)
	}
	if cards[0].Status != kanban.StatusPending {
		t.Errorf("Status = %q, want pending", cards[0].Status)
	}
}

// The handle identifies the pending timer, not a card: at the time
// After returns there is no card to name. The old API returned this
// value as a card id, which no lookup could ever resolve.
func TestAfterHandleIsNotACardID(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	handle, err := s.After(context.Background(), 5*time.Millisecond, task("worker"))
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if _, ok := k.Card(handle); ok {
		t.Fatal("the delay handle resolved as a card id")
	}

	cards := waitForCards(t, k, 1)
	if cards[0].ID == handle {
		t.Error("card id equals the delay handle")
	}
}

func TestAfterPreservesProducer(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)
	ctx := kanban.WithProducerID(context.Background(), "planner")

	if _, err := s.After(ctx, 5*time.Millisecond, task("worker")); err != nil {
		t.Fatalf("After: %v", err)
	}
	cards := waitForCards(t, k, 1)
	if cards[0].Producer != "planner" {
		t.Errorf("Producer = %q, want planner — the caller's identity must survive the timer",
			cards[0].Producer)
	}
}

func TestCancelDelay(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	handle, err := s.After(context.Background(), time.Hour, task("worker"))
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if !s.CancelDelay(handle) {
		t.Fatal("CancelDelay returned false for a pending delay")
	}
	if s.CancelDelay(handle) {
		t.Error("CancelDelay returned true twice")
	}
	if s.CancelDelay("nonexistent") {
		t.Error("CancelDelay returned true for an unknown handle")
	}
	if k.Len() != 0 {
		t.Error("a cancelled delay still submitted")
	}
}

func TestAfterValidates(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.After(context.Background(), -time.Second, task("w")); !errdefs.IsValidation(err) {
		t.Errorf("negative delay: err = %v, want validation", err)
	}
	if _, err := s.After(context.Background(), time.Second, kanban.Task{}); !errdefs.IsValidation(err) {
		t.Errorf("missing target: err = %v, want validation", err)
	}
}

func TestAddValidates(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)
	ctx := context.Background()

	if _, err := s.Add(ctx, scheduler.Rule{Task: task("w")}); !errdefs.IsValidation(err) {
		t.Errorf("missing cron: err = %v, want validation", err)
	}
	if _, err := s.Add(ctx, scheduler.Rule{Cron: "* * * * *"}); !errdefs.IsValidation(err) {
		t.Errorf("missing target: err = %v, want validation", err)
	}
	if _, err := s.Add(ctx, scheduler.Rule{
		Cron: "not a cron expression",
		Task: task("w"),
	}); !errdefs.IsValidation(err) {
		t.Errorf("malformed cron: err = %v, want validation", err)
	}
}

func TestAddAndRemoveRule(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)
	ctx := context.Background()

	id, err := s.Add(ctx, scheduler.Rule{Cron: "@every 1h", Task: task("worker")})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := s.Rules(); len(got) != 1 || got[0] != id {
		t.Fatalf("Rules = %v, want [%s]", got, id)
	}
	if !s.Remove(ctx, id) {
		t.Error("Remove returned false for an armed rule")
	}
	if s.Remove(ctx, id) {
		t.Error("Remove returned true twice")
	}
	if got := s.Rules(); len(got) != 0 {
		t.Errorf("Rules = %v, want empty", got)
	}
}

func TestAddHonoursSuppliedID(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	id, err := s.Add(context.Background(), scheduler.Rule{
		ID:   "nightly-digest",
		Cron: "@every 1h",
		Task: task("worker"),
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != "nightly-digest" {
		t.Errorf("id = %q, want the supplied id", id)
	}
}

func TestCronRuleFiresAndTagsCard(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	id, err := s.Add(context.Background(), scheduler.Rule{
		Cron:    "@every 10ms",
		Task:    task("worker"),
		Overlap: scheduler.OverlapAllow,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()

	cards := waitForCards(t, k, 1)
	if got := cards[0].Meta[scheduler.MetaScheduleID]; got != id {
		t.Errorf("card schedule_id = %q, want %q", got, id)
	}
}

// OverlapSkip is the default because a recurring job that falls behind
// must not queue work faster than it drains.
func TestOverlapSkipWaitsForOutstandingCard(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron: "@every 10ms",
		Task: task("worker"),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()

	waitForCards(t, k, 1)
	time.Sleep(120 * time.Millisecond) // ~12 triggers

	if got := k.Len(); got != 1 {
		t.Errorf("board holds %d cards, want 1 — skip policy did not suppress overlapping triggers", got)
	}
}

// A suspended card is paused work, not finished work: firing again
// would duplicate it.
func TestOverlapSkipCountsSuspendedCards(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron: "@every 10ms",
		Task: task("worker"),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()

	cards := waitForCards(t, k, 1)
	k.Claim(cards[0].ID, "worker-1")
	k.Suspend(cards[0].ID, "ckpt")

	time.Sleep(80 * time.Millisecond)
	if got := k.Len(); got != 1 {
		t.Errorf("board holds %d cards, want 1 — a suspended card must block the next trigger", got)
	}
}

func TestOverlapSkipResumesAfterTerminal(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron: "@every 10ms",
		Task: task("worker"),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()

	cards := waitForCards(t, k, 1)
	k.Claim(cards[0].ID, "worker-1")
	k.Done(cards[0].ID, kanban.Result{Output: "ok"})

	waitForCards(t, k, 2)
}

func TestOverlapAllowStacksCards(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron:    "@every 10ms",
		Task:    task("worker"),
		Overlap: scheduler.OverlapAllow,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.Start()

	waitForCards(t, k, 3)
}

func TestCloseStopsEverything(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron:    "@every 10ms",
		Task:    task("worker"),
		Overlap: scheduler.OverlapAllow,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.After(context.Background(), 50*time.Millisecond, task("worker")); err != nil {
		t.Fatalf("After: %v", err)
	}
	s.Start()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	before := k.Len()
	time.Sleep(100 * time.Millisecond)
	if after := k.Len(); after != before {
		t.Errorf("board grew from %d to %d after Close", before, after)
	}
}

func TestAfterOnClosedScheduler(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)
	_ = s.Close()

	if _, err := s.After(context.Background(), time.Millisecond, task("w")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want not available", err)
	}
}

// ---- RuleStore ----

type memStore struct {
	mu      sync.Mutex
	rules   map[string]scheduler.Rule
	listErr error
}

func newMemStore() *memStore {
	return &memStore{rules: make(map[string]scheduler.Rule)}
}

func (m *memStore) Save(_ context.Context, r scheduler.Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[r.ID] = r
	return nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rules, id)
	return nil
}

func (m *memStore) List(_ context.Context) ([]scheduler.Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]scheduler.Rule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rules)
}

func TestRuleStorePersistsAndDeletes(t *testing.T) {
	k := newBoard(t)
	store := newMemStore()
	s := newSched(t, k, scheduler.WithRuleStore(store))
	ctx := context.Background()

	id, err := s.Add(ctx, scheduler.Rule{Cron: "@every 1h", Task: task("worker")})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if store.len() != 1 {
		t.Fatalf("store holds %d rules, want 1", store.len())
	}
	s.Remove(ctx, id)
	if store.len() != 0 {
		t.Errorf("store holds %d rules after Remove, want 0", store.len())
	}
}

func TestAddDoesNotPersistAnUnarmableRule(t *testing.T) {
	k := newBoard(t)
	store := newMemStore()
	s := newSched(t, k, scheduler.WithRuleStore(store))

	if _, err := s.Add(context.Background(), scheduler.Rule{
		Cron: "garbage",
		Task: task("worker"),
	}); err == nil {
		t.Fatal("Add accepted a malformed cron expression")
	}
	if store.len() != 0 {
		t.Errorf("store holds %d rules, want 0 — a rule that cannot be armed must not persist", store.len())
	}
}

func TestRestoreRearmsStoredRules(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	// First process: register a rule, then go away.
	k1 := newBoard(t)
	s1 := newSched(t, k1, scheduler.WithRuleStore(store))
	id, err := s1.Add(ctx, scheduler.Rule{
		ID:      "nightly",
		Cron:    "@every 10ms",
		Task:    task("worker"),
		Overlap: scheduler.OverlapAllow,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = s1.Close()

	// Second process: same store, fresh board and scheduler.
	k2 := newBoard(t)
	s2 := newSched(t, k2, scheduler.WithRuleStore(store))
	n, err := s2.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 1 {
		t.Fatalf("Restore armed %d rules, want 1", n)
	}
	if got := s2.Rules(); len(got) != 1 || got[0] != id {
		t.Fatalf("Rules = %v, want [%s]", got, id)
	}
	s2.Start()
	waitForCards(t, k2, 1)
}

// One unparsable row must not stop a process from booting.
func TestRestoreSkipsUnusableRules(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	_ = store.Save(ctx, scheduler.Rule{ID: "good", Cron: "@every 1h", Task: task("worker")})
	_ = store.Save(ctx, scheduler.Rule{ID: "bad", Cron: "nonsense", Task: task("worker")})

	k := newBoard(t)
	s := newSched(t, k, scheduler.WithRuleStore(store))

	n, err := s.Restore(ctx)
	if n != 1 {
		t.Errorf("Restore armed %d rules, want 1", n)
	}
	if err == nil {
		t.Error("Restore hid the unusable rule; the error must be reported")
	}
	if got := s.Rules(); len(got) != 1 || got[0] != "good" {
		t.Errorf("Rules = %v, want [good]", got)
	}
}

func TestRestoreWithoutStoreIsANoop(t *testing.T) {
	k := newBoard(t)
	s := newSched(t, k)
	n, err := s.Restore(context.Background())
	if n != 0 || err != nil {
		t.Errorf("Restore = (%d, %v), want (0, nil)", n, err)
	}
}
