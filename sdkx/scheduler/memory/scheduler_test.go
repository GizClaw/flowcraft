package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
)

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

type maintenanceImpl struct {
	mu sync.Mutex

	compactRequests []sdkmemory.CompactRequest
	archiveRequests []sdkmemory.ArchiveRequest
	err             error
	block           bool
	entered         chan Operation
	release         chan struct{}
	active          int
	overlapped      bool
}

func (m *maintenanceImpl) CompileCompact(context.Context, sdkmemory.CompactRequest) sdkmemory.CompileResult {
	return native(sdkmemory.OpCompact,
		sdkmemory.FieldCompactScope, sdkmemory.FieldCompactOlderThan, sdkmemory.FieldCompactKeep)
}

func (m *maintenanceImpl) ExecuteCompact(ctx context.Context, request sdkmemory.CompactRequest) (sdkmemory.CompactResponse, error) {
	if err := m.enter(ctx, OperationCompact); err != nil {
		return sdkmemory.CompactResponse{}, err
	}
	m.mu.Lock()
	m.compactRequests = append(m.compactRequests, request)
	err := m.err
	m.mu.Unlock()
	return sdkmemory.CompactResponse{}, err
}

func (m *maintenanceImpl) CompileArchive(context.Context, sdkmemory.ArchiveRequest) sdkmemory.CompileResult {
	return native(sdkmemory.OpArchive,
		sdkmemory.FieldArchiveScope, sdkmemory.FieldArchiveOlderThan, sdkmemory.FieldArchiveDestination)
}

func (m *maintenanceImpl) ExecuteArchive(ctx context.Context, request sdkmemory.ArchiveRequest) (sdkmemory.ArchiveResponse, error) {
	if err := m.enter(ctx, OperationArchive); err != nil {
		return sdkmemory.ArchiveResponse{}, err
	}
	m.mu.Lock()
	m.archiveRequests = append(m.archiveRequests, request)
	err := m.err
	m.mu.Unlock()
	return sdkmemory.ArchiveResponse{}, err
}

func (m *maintenanceImpl) enter(ctx context.Context, operation Operation) error {
	m.mu.Lock()
	if !m.block {
		m.mu.Unlock()
		return nil
	}
	m.active++
	if m.active > 1 {
		m.overlapped = true
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		m.mu.Unlock()
	}()
	m.entered <- operation
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *maintenanceImpl) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.compactRequests), len(m.archiveRequests)
}

func native(operation sdkmemory.Operation, fields ...sdkmemory.FieldID) sdkmemory.CompileResult {
	decisions := make([]sdkmemory.Decision, len(fields))
	for index, field := range fields {
		decisions[index] = sdkmemory.Decision{
			Field:       field,
			Disposition: sdkmemory.DispositionNative,
		}
	}
	return sdkmemory.CompileResult{Op: operation, Decisions: decisions}
}

func newRuntime(t *testing.T, impl *maintenanceImpl, clock sdkmemory.Clock) *sdkmemory.Runtime {
	t.Helper()
	runtime, err := sdkmemory.New(sdkmemory.Spec{
		RuntimeID: "prod",
		DefaultScope: sdkmemory.Scope{
			RuntimeID: "prod",
			UserID:    "tenant",
		},
		Clock: clock,
	}, sdkmemory.Impls{
		Compact: impl,
		Archive: impl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestDispatcherMapsCompactAndArchiveTasks(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	impl := &maintenanceImpl{}
	scheduler, err := New(newRuntime(t, impl, fixedClock{now: now}), config.LifecycleSpec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })

	compactWork, err := scheduler.dispatcher.Dispatch(t.Context(), "compact-test", Task{
		Operation: OperationCompact,
		OlderThan: 7 * 24 * time.Hour,
		Keep:      50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outstanding, err := compactWork.IsOutstanding(t.Context()); err != nil || outstanding {
		t.Fatalf("compact outstanding = %v, error = %v", outstanding, err)
	}
	archiveWork, err := scheduler.dispatcher.Dispatch(t.Context(), "archive-test", Task{
		Operation:   OperationArchive,
		OlderThan:   90 * 24 * time.Hour,
		Destination: "s3://bucket/archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outstanding, err := archiveWork.IsOutstanding(t.Context()); err != nil || outstanding {
		t.Fatalf("archive outstanding = %v, error = %v", outstanding, err)
	}

	impl.mu.Lock()
	defer impl.mu.Unlock()
	compact := impl.compactRequests[0]
	if compact.Scope.RuntimeID != "prod" || compact.Scope.UserID != "tenant" ||
		!compact.OlderThan.Equal(now.Add(-7*24*time.Hour)) || compact.Keep != 50 {
		t.Fatalf("compact request = %+v", compact)
	}
	archive := impl.archiveRequests[0]
	if archive.Scope.RuntimeID != "prod" || archive.Scope.UserID != "tenant" ||
		!archive.OlderThan.Equal(now.Add(-90*24*time.Hour)) ||
		archive.Destination != "s3://bucket/archive" {
		t.Fatalf("archive request = %+v", archive)
	}
}

func TestDispatcherUsesRuntimeIDWhenDefaultScopeIsZero(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	impl := &maintenanceImpl{}
	runtime, err := sdkmemory.New(sdkmemory.Spec{
		RuntimeID: "prod",
		Clock:     fixedClock{now: now},
	}, sdkmemory.Impls{Compact: impl})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	scheduler, err := New(runtime, config.LifecycleSpec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	if _, err := scheduler.dispatcher.Dispatch(t.Context(), "compact-test", Task{
		Operation: OperationCompact,
		OlderThan: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if len(impl.compactRequests) != 1 ||
		impl.compactRequests[0].Scope != (sdkmemory.Scope{RuntimeID: "prod"}) {
		t.Fatalf("compact requests = %+v, want RuntimeID-only scope", impl.compactRequests)
	}
}

func TestNewRegistersEnabledCronRulesAndSkipsEmptyBlocks(t *testing.T) {
	impl := &maintenanceImpl{}
	runtime := newRuntime(t, impl, fixedClock{now: time.Now()})
	scheduler, err := New(runtime, config.LifecycleSpec{
		Compact: config.CompactLifecycleSpec{
			Cron:      "@hourly",
			OlderThan: 7 * 24 * time.Hour,
			Keep:      50,
		},
		Archive: config.ArchiveLifecycleSpec{
			Cron:        "0 3 * * *",
			OlderThan:   90 * 24 * time.Hour,
			Destination: "s3://bucket/archive",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	if got := scheduler.Rules(); !slices.Equal(got, []string{ArchiveRuleID, CompactRuleID}) {
		t.Fatalf("Rules = %v", got)
	}

	disabled, err := New(runtime, config.LifecycleSpec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disabled.Close() })
	if got := disabled.Rules(); len(got) != 0 {
		t.Fatalf("disabled Rules = %v", got)
	}
}

func TestDispatcherSerializesCompactAndArchiveAcrossRules(t *testing.T) {
	impl := &maintenanceImpl{
		block:   true,
		entered: make(chan Operation, 2),
		release: make(chan struct{}, 2),
	}
	scheduler, err := New(newRuntime(t, impl, fixedClock{now: time.Now()}), config.LifecycleSpec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })

	done := make(chan error, 2)
	go func() {
		_, err := scheduler.dispatcher.Dispatch(context.Background(), "compact", Task{
			Operation: OperationCompact, OlderThan: time.Hour,
		})
		done <- err
	}()
	select {
	case operation := <-impl.entered:
		if operation != OperationCompact {
			t.Fatalf("first operation = %q", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("compact did not start")
	}
	go func() {
		_, err := scheduler.dispatcher.Dispatch(context.Background(), "archive", Task{
			Operation: OperationArchive, OlderThan: time.Hour, Destination: "memory://archive",
		})
		done <- err
	}()
	select {
	case operation := <-impl.entered:
		t.Fatalf("%s overlapped compact", operation)
	case <-time.After(50 * time.Millisecond):
	}
	impl.release <- struct{}{}
	select {
	case operation := <-impl.entered:
		if operation != OperationArchive {
			t.Fatalf("second operation = %q", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("archive did not start after compact")
	}
	impl.release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if impl.overlapped {
		t.Fatal("maintenance operations overlapped")
	}
}

func TestCloseCancelsBlockedExecute(t *testing.T) {
	impl := &maintenanceImpl{
		block:   true,
		entered: make(chan Operation, 1),
		release: make(chan struct{}),
	}
	scheduler, err := New(newRuntime(t, impl, fixedClock{now: time.Now()}), config.LifecycleSpec{
		Compact: config.CompactLifecycleSpec{
			Cron:      "@every 1s",
			OlderThan: time.Hour,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Start()
	select {
	case <-impl.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("compact did not start")
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked ExecuteCompact")
	}
}

func TestNewRejectsNilRuntimeAndInvalidLifecycle(t *testing.T) {
	if _, err := New(nil, config.LifecycleSpec{}); !errdefs.IsValidation(err) {
		t.Fatalf("New(nil) error = %v", err)
	}
	runtime := newRuntime(t, &maintenanceImpl{}, fixedClock{now: time.Now()})
	if _, err := New(runtime, config.LifecycleSpec{
		Compact: config.CompactLifecycleSpec{Cron: "@hourly"},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("incomplete lifecycle error = %v", err)
	}
}

type ruleStore struct {
	called bool
}

func (s *ruleStore) Save(context.Context, Rule) error {
	s.called = true
	return nil
}

func (s *ruleStore) Delete(context.Context, string) error {
	s.called = true
	return nil
}

func (s *ruleStore) List(context.Context) ([]Rule, error) {
	s.called = true
	return nil, nil
}

func TestNewDoesNotPerformRuleStoreIO(t *testing.T) {
	store := &ruleStore{}
	runtime := newRuntime(t, &maintenanceImpl{}, fixedClock{now: time.Now()})
	_, err := New(runtime, config.LifecycleSpec{
		Compact: config.CompactLifecycleSpec{
			Cron:      "@hourly",
			OlderThan: time.Hour,
		},
	}, WithRuleStore(store))
	if !errdefs.IsValidation(err) {
		t.Fatalf("New error = %v, want validation", err)
	}
	if store.called {
		t.Fatal("New performed rule store I/O")
	}
}

type persistedRuleStore struct {
	rules []Rule
}

func (*persistedRuleStore) Save(context.Context, Rule) error     { return nil }
func (*persistedRuleStore) Delete(context.Context, string) error { return nil }
func (s *persistedRuleStore) List(context.Context) ([]Rule, error) {
	return slices.Clone(s.rules), nil
}

func TestRestoreValidatesPersistedMemoryTasks(t *testing.T) {
	store := &persistedRuleStore{rules: []Rule{
		{
			ID: "valid", Cron: "@hourly",
			Value: Task{Operation: OperationCompact, OlderThan: time.Hour},
		},
		{
			ID: "invalid", Cron: "@hourly",
			Value: Task{Operation: OperationArchive, OlderThan: time.Hour},
		},
	}}
	runtime := newRuntime(t, &maintenanceImpl{}, fixedClock{now: time.Now()})
	scheduler, err := New(runtime, config.LifecycleSpec{}, WithRuleStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	n, err := scheduler.Restore(t.Context())
	if n != 1 || !errdefs.IsValidation(err) {
		t.Fatalf("Restore = (%d, %v), want one plus validation", n, err)
	}
	if got := scheduler.Rules(); !slices.Equal(got, []string{"valid"}) {
		t.Fatalf("Rules = %v, want [valid]", got)
	}
}

func TestExecuteErrorDoesNotStopScheduler(t *testing.T) {
	impl := &maintenanceImpl{err: errors.New("storage unavailable")}
	scheduler, err := New(newRuntime(t, impl, fixedClock{now: time.Now()}), config.LifecycleSpec{
		Compact: config.CompactLifecycleSpec{
			Cron:      "@every 1s",
			OlderThan: time.Hour,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	scheduler.Start()
	waitFor(t, 2*time.Second, func() bool {
		compact, _ := impl.counts()
		return compact >= 1
	})
	impl.mu.Lock()
	impl.err = nil
	impl.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool {
		compact, _ := impl.counts()
		return compact >= 2
	})
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
