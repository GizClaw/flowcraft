package memory

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
	localscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
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
	entered         chan string
	release         chan struct{}
	active          int
	overlapped      bool
}

func (m *maintenanceImpl) CompileCompact(context.Context, sdkmemory.CompactRequest) sdkmemory.CompileResult {
	return native(sdkmemory.OpCompact,
		sdkmemory.FieldCompactScope, sdkmemory.FieldCompactOlderThan, sdkmemory.FieldCompactKeep)
}

func (m *maintenanceImpl) ExecuteCompact(
	ctx context.Context,
	request sdkmemory.CompactRequest,
) (sdkmemory.CompactResponse, error) {
	if err := m.enter(ctx, "compact"); err != nil {
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

func (m *maintenanceImpl) ExecuteArchive(
	ctx context.Context,
	request sdkmemory.ArchiveRequest,
) (sdkmemory.ArchiveResponse, error) {
	if err := m.enter(ctx, "archive"); err != nil {
		return sdkmemory.ArchiveResponse{}, err
	}
	m.mu.Lock()
	m.archiveRequests = append(m.archiveRequests, request)
	err := m.err
	m.mu.Unlock()
	return sdkmemory.ArchiveResponse{}, err
}

func (m *maintenanceImpl) enter(ctx context.Context, operation string) error {
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
	select {
	case m.entered <- operation:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

func newLocalServer(t *testing.T) *localscheduler.LocalServer {
	t.Helper()
	server, err := localscheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func newScheduler(
	t *testing.T,
	server sdkscheduler.Server,
	namespace string,
	runtime *sdkmemory.Runtime,
	lifecycle LifecycleSpec,
) *Scheduler {
	t.Helper()
	scheduler, err := New(
		t.Context(), server, namespace, runtime, lifecycle,
		WithWorkerOptions(sdkscheduler.WithPollInterval(time.Millisecond)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	return scheduler
}

func start(t *testing.T, scheduler *Scheduler, server *localscheduler.LocalServer) {
	t.Helper()
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
}

func TestNewRegistersLifecycleRules(t *testing.T) {
	server := newLocalServer(t)
	runtime := newRuntime(t, &maintenanceImpl{}, fixedClock{now: time.Now()})
	scheduler := newScheduler(t, server, "memory", runtime, config.LifecycleSpec{
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

	rules, err := scheduler.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(rules))
	}
	if got := []string{rules[0].ID, rules[1].ID}; !slices.Equal(got, []string{ArchiveRuleID, CompactRuleID}) {
		t.Fatalf("rule IDs = %v", got)
	}
	for _, rule := range rules {
		if rule.Namespace != "memory" || rule.Timezone != "UTC" ||
			rule.Overlap != sdkscheduler.OverlapSkip {
			t.Fatalf("rule = %+v", rule)
		}
	}

	empty := newScheduler(t, server, "disabled", runtime, config.LifecycleSpec{})
	emptyRules, err := empty.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyRules) != 0 {
		t.Fatalf("disabled rules = %+v", emptyRules)
	}
}

func TestWorkerClaimsAndExecutesMaintenance(t *testing.T) {
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	impl := &maintenanceImpl{}
	server := newLocalServer(t)
	scheduler := newScheduler(
		t, server, "memory", newRuntime(t, impl, fixedClock{now: now}), config.LifecycleSpec{},
	)
	start(t, scheduler, server)

	if _, err := scheduler.After(t.Context(), 0, Task{Compact: &CompactTask{
		OlderThan: 7 * 24 * time.Hour,
		Keep:      50,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.After(t.Context(), 0, Task{Archive: &ArchiveTask{
		OlderThan:   90 * 24 * time.Hour,
		Destination: "s3://bucket/archive",
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		return len(impl.compactRequests) == 1 && len(impl.archiveRequests) == 1
	})

	impl.mu.Lock()
	defer impl.mu.Unlock()
	compact := impl.compactRequests[0]
	if compact.Scope != (sdkmemory.Scope{RuntimeID: "prod", UserID: "tenant"}) ||
		!compact.OlderThan.Equal(now.Add(-7*24*time.Hour)) || compact.Keep != 50 {
		t.Fatalf("compact request = %+v", compact)
	}
	archive := impl.archiveRequests[0]
	if archive.Scope != (sdkmemory.Scope{RuntimeID: "prod", UserID: "tenant"}) ||
		!archive.OlderThan.Equal(now.Add(-90*24*time.Hour)) ||
		archive.Destination != "s3://bucket/archive" {
		t.Fatalf("archive request = %+v", archive)
	}
}

func TestLifecycleRuleIsClaimedAndExecuted(t *testing.T) {
	impl := &maintenanceImpl{}
	server := newLocalServer(t)
	scheduler := newScheduler(
		t,
		server,
		"memory",
		newRuntime(t, impl, fixedClock{now: time.Now()}),
		config.LifecycleSpec{Compact: config.CompactLifecycleSpec{
			Cron:      "@every 1s",
			OlderThan: time.Hour,
		}},
	)
	start(t, scheduler, server)
	waitFor(t, 2*time.Second, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		return len(impl.compactRequests) != 0
	})
}

func TestWorkerSerializesCompactAndArchive(t *testing.T) {
	impl := &maintenanceImpl{
		block:   true,
		entered: make(chan string, 2),
		release: make(chan struct{}, 2),
	}
	server := newLocalServer(t)
	scheduler := newScheduler(
		t, server, "memory", newRuntime(t, impl, fixedClock{now: time.Now()}), config.LifecycleSpec{},
	)
	start(t, scheduler, server)

	if _, err := scheduler.After(t.Context(), 0, Task{
		Compact: &CompactTask{OlderThan: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.After(t.Context(), 0, Task{
		Archive: &ArchiveTask{OlderThan: time.Hour, Destination: "memory://archive"},
	}); err != nil {
		t.Fatal(err)
	}
	first := receive(t, impl.entered)
	select {
	case operation := <-impl.entered:
		t.Fatalf("%s overlapped %s", operation, first)
	case <-time.After(50 * time.Millisecond):
	}
	impl.release <- struct{}{}
	second := receive(t, impl.entered)
	if first == second {
		t.Fatalf("operations = %q then %q", first, second)
	}
	impl.release <- struct{}{}
	waitFor(t, time.Second, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		return len(impl.compactRequests) == 1 && len(impl.archiveRequests) == 1
	})
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if impl.overlapped {
		t.Fatal("maintenance operations overlapped")
	}
}

type observingServer struct {
	*localscheduler.LocalServer
	completed chan sdkscheduler.CompleteRequest
}

func (s *observingServer) Complete(ctx context.Context, request sdkscheduler.CompleteRequest) error {
	err := s.LocalServer.Complete(ctx, request)
	if err == nil {
		s.completed <- request
	}
	return err
}

func TestOperationFailureCompletesExecutionFailed(t *testing.T) {
	local := newLocalServer(t)
	server := &observingServer{
		LocalServer: local,
		completed:   make(chan sdkscheduler.CompleteRequest, 1),
	}
	impl := &maintenanceImpl{err: errors.New("storage unavailable")}
	scheduler := newScheduler(
		t, server, "memory", newRuntime(t, impl, fixedClock{now: time.Now()}), config.LifecycleSpec{},
	)
	start(t, scheduler, local)
	if _, err := scheduler.After(t.Context(), 0, Task{
		Compact: &CompactTask{OlderThan: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-server.completed:
		if completion.Status != sdkscheduler.StatusFailed ||
			!strings.Contains(completion.Error, "compact") {
			t.Fatalf("completion = %+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("execution was not completed")
	}
}

func TestCloseCancelsBlockedIOAndKeepsBorrowedResourcesOpen(t *testing.T) {
	impl := &maintenanceImpl{
		block:   true,
		entered: make(chan string, 1),
		release: make(chan struct{}),
	}
	server := newLocalServer(t)
	runtime := newRuntime(t, impl, fixedClock{now: time.Now()})
	scheduler := newScheduler(t, server, "memory", runtime, config.LifecycleSpec{})
	start(t, scheduler, server)
	if _, err := scheduler.After(t.Context(), 0, Task{
		Compact: &CompactTask{OlderThan: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	receive(t, impl.entered)

	done := make(chan error, 1)
	go func() { done <- scheduler.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked maintenance I/O")
	}
	if err := scheduler.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := server.ListRules(t.Context(), "memory"); err != nil {
		t.Fatalf("borrowed server was closed: %v", err)
	}
	impl.mu.Lock()
	impl.block = false
	impl.mu.Unlock()
	if _, err := runtime.ExecuteCompact(t.Context(), sdkmemory.CompactRequest{
		Scope:     sdkmemory.Scope{RuntimeID: "prod", UserID: "tenant"},
		OlderThan: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("borrowed runtime was closed or canceled: %v", err)
	}
}

func TestStartDoesNotDependOnNewContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := newLocalServer(t)
	impl := &maintenanceImpl{}
	scheduler, err := New(
		ctx,
		server,
		"memory",
		newRuntime(t, impl, fixedClock{now: time.Now()}),
		config.LifecycleSpec{},
		WithWorkerOptions(sdkscheduler.WithPollInterval(time.Millisecond)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	cancel()
	start(t, scheduler, server)
	if _, err := scheduler.After(t.Context(), 0, Task{
		Compact: &CompactTask{OlderThan: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		impl.mu.Lock()
		defer impl.mu.Unlock()
		return len(impl.compactRequests) == 1
	})
	if err := scheduler.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
}

func TestValidation(t *testing.T) {
	server := newLocalServer(t)
	runtime := newRuntime(t, &maintenanceImpl{}, fixedClock{now: time.Now()})
	var nilServer *localscheduler.LocalServer
	tests := []struct {
		name string
		call func() error
	}{
		{"nil context", func() error {
			_, err := New(nil, server, "memory", runtime, config.LifecycleSpec{})
			return err
		}},
		{"nil server", func() error {
			_, err := New(t.Context(), nil, "memory", runtime, config.LifecycleSpec{})
			return err
		}},
		{"typed nil server", func() error {
			_, err := New(t.Context(), nilServer, "memory", runtime, config.LifecycleSpec{})
			return err
		}},
		{"empty namespace", func() error {
			_, err := New(t.Context(), server, " ", runtime, config.LifecycleSpec{})
			return err
		}},
		{"nil runtime", func() error {
			_, err := New(t.Context(), server, "memory", nil, config.LifecycleSpec{})
			return err
		}},
		{"invalid lifecycle", func() error {
			_, err := New(t.Context(), server, "memory", runtime, config.LifecycleSpec{
				Compact: config.CompactLifecycleSpec{Cron: "@hourly"},
			})
			return err
		}},
		{"nil option", func() error {
			_, err := New(t.Context(), server, "memory", runtime, config.LifecycleSpec{}, nil)
			return err
		}},
		{"nil clock", func() error {
			_, err := New(
				t.Context(), server, "memory", runtime, config.LifecycleSpec{}, WithClock(nil),
			)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}

	invalidTasks := []Task{
		{},
		{Compact: &CompactTask{OlderThan: time.Hour}, Archive: &ArchiveTask{
			OlderThan: time.Hour, Destination: "archive",
		}},
		{Compact: &CompactTask{}},
		{Compact: &CompactTask{OlderThan: time.Hour, Keep: -1}},
		{Archive: &ArchiveTask{OlderThan: time.Hour}},
	}
	for _, task := range invalidTasks {
		if err := task.Validate(); !errdefs.IsValidation(err) {
			t.Fatalf("Task.Validate(%+v) = %v, want validation", task, err)
		}
	}

	scheduler := newScheduler(t, server, "task-validation", runtime, config.LifecycleSpec{})
	if _, err := scheduler.PutRule(t.Context(), Rule{
		ID:   "invalid",
		Cron: "@hourly",
		Task: Task{},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("PutRule invalid task error = %v, want validation", err)
	}
	if _, err := scheduler.After(t.Context(), 0, Task{}); !errdefs.IsValidation(err) {
		t.Fatalf("After invalid task error = %v, want validation", err)
	}
}

func receive(t *testing.T, channel <-chan string) string {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for maintenance operation")
		return ""
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
