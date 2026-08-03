package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/scheduler"
)

type dispatch struct {
	mu       sync.Mutex
	values   []string
	ids      []string
	pending  map[string]bool
	err      error
	checkErr error
	block    chan struct{}
	entered  chan struct{}
}

func newDispatch() *dispatch {
	return &dispatch{pending: make(map[string]bool)}
}

func (d *dispatch) Dispatch(_ context.Context, id, value string) (scheduler.Outstanding, error) {
	if d.entered != nil {
		d.entered <- struct{}{}
	}
	if d.block != nil {
		<-d.block
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	d.ids = append(d.ids, id)
	d.values = append(d.values, value)
	d.pending[id] = true
	return dispatchedWork{dispatch: d, id: id}, nil
}

type dispatchedWork struct {
	dispatch *dispatch
	id       string
}

func (w dispatchedWork) IsOutstanding(context.Context) (bool, error) {
	w.dispatch.mu.Lock()
	defer w.dispatch.mu.Unlock()
	return w.dispatch.pending[w.id], w.dispatch.checkErr
}

func (d *dispatch) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.values)
}

func (d *dispatch) complete(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[id] = false
}

func newScheduler(t *testing.T, d *dispatch, opts ...scheduler.Option[string]) *scheduler.Scheduler[string] {
	t.Helper()
	s, err := scheduler.New[string](d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitCount(t *testing.T, d *dispatch, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for d.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("dispatch count = %d, want at least %d", d.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	if _, err := scheduler.New[string](nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil dispatcher error = %v", err)
	}
	var store *memStore
	if _, err := scheduler.New[string](newDispatch(), scheduler.WithRuleStore[string](store)); !errdefs.IsValidation(err) {
		t.Fatalf("typed nil store error = %v", err)
	}
}

func TestAfterAndCancel(t *testing.T) {
	d := newDispatch()
	s := newScheduler(t, d)
	handle, err := s.After(context.Background(), 5*time.Millisecond, "once")
	if err != nil || handle == "" {
		t.Fatalf("After = (%q, %v)", handle, err)
	}
	waitCount(t, d, 1)

	cancel, err := s.After(context.Background(), time.Hour, "never")
	if err != nil {
		t.Fatal(err)
	}
	if !s.CancelDelay(cancel) || s.CancelDelay(cancel) {
		t.Fatal("CancelDelay did not report pending state exactly once")
	}
	if _, err := s.After(context.Background(), -time.Second, "bad"); !errdefs.IsValidation(err) {
		t.Fatalf("negative delay error = %v", err)
	}
}

func TestCronOverlapSkipTracksBusinessStatus(t *testing.T) {
	d := newDispatch()
	s := newScheduler(t, d)
	id, err := s.Add(context.Background(), scheduler.Rule[string]{
		ID: "job", Cron: "@every 5ms", Value: "work",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	waitCount(t, d, 1)
	time.Sleep(35 * time.Millisecond)
	if got := d.count(); got != 1 {
		t.Fatalf("dispatches while outstanding = %d, want 1", got)
	}
	d.complete(id)
	waitCount(t, d, 2)
}

func TestOverlapSkipWaitsForSubmissionThenChecksBusinessStatus(t *testing.T) {
	d := newDispatch()
	d.block = make(chan struct{})
	s := newScheduler(t, d)
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		ID: "slow", Cron: "@every 5ms", Value: "work",
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(15 * time.Millisecond)
	close(d.block)
	waitCount(t, d, 1)
	time.Sleep(25 * time.Millisecond)
	if got := d.count(); got != 1 {
		t.Fatalf("dispatches = %d, want 1; callback submission is not the outstanding state", got)
	}
}

func TestCronOverlapAllow(t *testing.T) {
	d := newDispatch()
	s := newScheduler(t, d)
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		Cron: "@every 5ms", Value: "work", Overlap: scheduler.OverlapAllow,
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	waitCount(t, d, 3)
}

func TestOutstandingErrorSkipsConservatively(t *testing.T) {
	d := newDispatch()
	d.checkErr = errors.New("status unavailable")
	s := newScheduler(t, d)
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		Cron: "@every 5ms", Value: "work",
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(20 * time.Millisecond)
	if got := d.count(); got != 0 {
		t.Fatalf("dispatches = %d, want 0", got)
	}
}

func TestTimezoneAndValidation(t *testing.T) {
	d := newDispatch()
	s := newScheduler(t, d)
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		Cron: "* * * * *", Timezone: "Not/AZone", Value: "work",
	}); !errdefs.IsValidation(err) {
		t.Fatalf("timezone error = %v", err)
	}
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		Cron: "* * * * *", Overlap: "invalid", Value: "work",
	}); !errdefs.IsValidation(err) {
		t.Fatalf("overlap error = %v", err)
	}
}

type memStore struct {
	mu        sync.Mutex
	rules     map[string]scheduler.Rule[string]
	listErr   error
	deleteErr error
}

func newStore() *memStore {
	return &memStore{rules: make(map[string]scheduler.Rule[string])}
}

func (m *memStore) Save(_ context.Context, rule scheduler.Rule[string]) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[rule.ID] = rule
	return nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.rules, id)
	return nil
}

func (m *memStore) List(context.Context) ([]scheduler.Rule[string], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]scheduler.Rule[string], 0, len(m.rules))
	for _, rule := range m.rules {
		out = append(out, rule)
	}
	return out, nil
}

type blockingSaveStore struct {
	*memStore
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (m *blockingSaveStore) Save(ctx context.Context, rule scheduler.Rule[string]) error {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 2 {
		close(m.started)
		<-m.release
	}
	return m.memStore.Save(ctx, rule)
}

func TestRulePersistenceRestoreAndRemove(t *testing.T) {
	store := newStore()
	first := newScheduler(t, newDispatch(), scheduler.WithRuleStore[string](store))
	id, err := first.Add(context.Background(), scheduler.Rule[string]{
		ID: "restored", Cron: "@every 5ms", Value: "work", Overlap: scheduler.OverlapAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	d := newDispatch()
	second := newScheduler(t, d, scheduler.WithRuleStore[string](store))
	n, err := second.Restore(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Restore = (%d, %v)", n, err)
	}
	if got := second.Rules(); len(got) != 1 || got[0] != id {
		t.Fatalf("Rules = %v", got)
	}
	second.Start()
	waitCount(t, d, 1)
	removed, err := second.Remove(context.Background(), id)
	if err != nil || !removed {
		t.Fatalf("first Remove = (%v, %v)", removed, err)
	}
	removed, err = second.Remove(context.Background(), id)
	if err != nil || removed {
		t.Fatal("Remove result was not true then false")
	}
}

func TestRemoveReturnsStoreDeleteError(t *testing.T) {
	store := newStore()
	s := newScheduler(t, newDispatch(), scheduler.WithRuleStore[string](store))
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		ID: "delete-error", Cron: "@hourly", Value: "work",
	}); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("delete failed")
	store.deleteErr = deleteErr
	removed, err := s.Remove(context.Background(), "delete-error")
	if !removed || !errors.Is(err, deleteErr) {
		t.Fatalf("Remove = (%v, %v), want true and delete error", removed, err)
	}
	if got := s.Rules(); len(got) != 0 {
		t.Fatalf("Rules after failed persisted delete = %v", got)
	}
}

func TestRestoreSkipsInvalidRules(t *testing.T) {
	store := newStore()
	store.rules["good"] = scheduler.Rule[string]{ID: "good", Cron: "@hourly", Value: "ok"}
	store.rules["bad"] = scheduler.Rule[string]{ID: "bad", Cron: "nonsense", Value: "bad"}
	s := newScheduler(t, newDispatch(), scheduler.WithRuleStore[string](store))
	n, err := s.Restore(context.Background())
	if n != 1 || err == nil {
		t.Fatalf("Restore = (%d, %v), want one plus error", n, err)
	}
}

func TestReplacementValidationFailurePreservesPersistedRule(t *testing.T) {
	store := newStore()
	s := newScheduler(t, newDispatch(), scheduler.WithRuleStore[string](store))
	old := scheduler.Rule[string]{ID: "replace", Cron: "@hourly", Value: "old"}
	if _, err := s.Add(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(context.Background(), scheduler.Rule[string]{
		ID: "replace", Cron: "invalid", Value: "new",
	}); !errdefs.IsValidation(err) {
		t.Fatalf("replacement error = %v, want validation", err)
	}
	store.mu.Lock()
	got := store.rules["replace"]
	store.mu.Unlock()
	if got != old {
		t.Fatalf("persisted rule = %+v, want %+v", got, old)
	}
}

func TestReplacementArmFailureRestoresPersistedRule(t *testing.T) {
	store := &blockingSaveStore{
		memStore: newStore(),
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	s := newScheduler(t, newDispatch(), scheduler.WithRuleStore[string](store))
	old := scheduler.Rule[string]{ID: "replace", Cron: "@hourly", Value: "old"}
	if _, err := s.Add(context.Background(), old); err != nil {
		t.Fatal(err)
	}

	addDone := make(chan error, 1)
	go func() {
		_, err := s.Add(context.Background(), scheduler.Rule[string]{
			ID: "replace", Cron: "@daily", Value: "new",
		})
		addDone <- err
	}()
	<-store.started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-addDone; !errdefs.IsNotAvailable(err) {
		t.Fatalf("replacement error = %v, want not available", err)
	}
	store.memStore.mu.Lock()
	got := store.rules["replace"]
	store.memStore.mu.Unlock()
	if got != old {
		t.Fatalf("persisted rule after failed replacement = %+v, want %+v", got, old)
	}
}

func TestCloseIsIdempotentAndRejectsNewWork(t *testing.T) {
	d := newDispatch()
	s := newScheduler(t, d)
	if _, err := s.After(context.Background(), 20*time.Millisecond, "late"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if d.count() != 0 {
		t.Fatal("delay fired after Close")
	}
	if _, err := s.After(context.Background(), 0, "closed"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("After after close error = %v", err)
	}
}

func TestCloseWaitsForAdmittedDelayDispatch(t *testing.T) {
	d := newDispatch()
	d.block = make(chan struct{})
	d.entered = make(chan struct{}, 1)
	s := newScheduler(t, d)
	if _, err := s.After(context.Background(), 0, "started"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delay callback did not enter Dispatch")
	}

	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while admitted Dispatch was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(d.block)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not wait for admitted Dispatch")
	}
	if got := d.count(); got != 1 {
		t.Fatalf("dispatch count after Close = %d, want 1", got)
	}
}

func TestReplacementSharesOverlapGateAcrossGenerations(t *testing.T) {
	d := newDispatch()
	d.block = make(chan struct{})
	d.entered = make(chan struct{}, 4)
	s := newScheduler(t, d)
	rule := scheduler.Rule[string]{ID: "replace", Cron: "@every 1s", Value: "old"}
	if _, err := s.Add(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case <-d.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old generation did not enter Dispatch")
	}
	rule.Value = "new"
	if _, err := s.Add(context.Background(), rule); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.entered:
		t.Fatal("replacement generation bypassed overlap gate")
	case <-time.After(1200 * time.Millisecond):
	}
	close(d.block)
	waitCount(t, d, 1)
}
