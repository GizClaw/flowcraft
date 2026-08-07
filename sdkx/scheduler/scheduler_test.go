package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
)

var testNow = time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC)

func task(value string) sdkscheduler.Task {
	return sdkscheduler.Task{Payload: sdkscheduler.Payload{
		Kind: "test", Version: 1, Data: []byte(`"` + value + `"`),
	}}
}

func rule(namespace, id string, overlap sdkscheduler.Overlap) sdkscheduler.Rule {
	return sdkscheduler.Rule{
		Namespace: namespace,
		ID:        id,
		Cron:      "@hourly",
		Timezone:  "UTC",
		Overlap:   overlap,
		Task:      task(id),
	}
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock { return &fakeClock{now: testNow} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(delay time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, at: c.now.Add(delay), fn: fn}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	var callbacks []func()
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired && !timer.at.After(c.now) {
			timer.fired = true
			callbacks = append(callbacks, timer.fn)
		}
	}
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

func newTestServer(t *testing.T) (*LocalServer, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	server, err := NewLocalServer(WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, clock
}

func claim(t *testing.T, server *LocalServer, namespace string, lease time.Duration) *sdkscheduler.Delivery {
	t.Helper()
	delivery, err := server.Claim(context.Background(), sdkscheduler.ClaimRequest{
		Namespace: namespace, LeaseDuration: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

func fireRuleForTest(t *testing.T, server *LocalServer, namespace, id string) {
	t.Helper()
	key := scheduleKey{namespace: namespace, id: id}
	server.mu.Lock()
	state := server.rules[key]
	server.mu.Unlock()
	if state == nil {
		t.Fatalf("missing rule %s/%s", namespace, id)
	}
	server.fireRule(key, state, state.generation)
}

func waitForServerClosed(t *testing.T, server *LocalServer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		closed := server.closed
		server.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not begin closing")
}

func TestLocalServerImplementsProtocolAndIsolatesNamespaces(t *testing.T) {
	server, _ := newTestServer(t)
	var protocol sdkscheduler.Server = server
	for _, namespace := range []string{"left", "right"} {
		if err := protocol.PutRule(context.Background(), rule(namespace, "shared", sdkscheduler.OverlapAllow)); err != nil {
			t.Fatal(err)
		}
		fireRuleForTest(t, server, namespace, "shared")
	}
	left := claim(t, server, "left", time.Minute)
	right := claim(t, server, "right", time.Minute)
	if left == nil || right == nil || left.Namespace != "left" || right.Namespace != "right" {
		t.Fatalf("namespace deliveries = (%+v, %+v)", left, right)
	}
	if extra := claim(t, server, "left", time.Minute); extra != nil {
		t.Fatalf("unexpected cross-namespace delivery: %+v", extra)
	}
}

func TestCronTriggerClaimComplete(t *testing.T) {
	server, err := NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	cronRule := rule("cron", "job", sdkscheduler.OverlapAllow)
	cronRule.Cron = "@every 5ms"
	if err := server.PutRule(context.Background(), cronRule); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	var delivery *sdkscheduler.Delivery
	deadline := time.Now().Add(3 * time.Second)
	for delivery == nil && time.Now().Before(deadline) {
		delivery = claim(t, server, "cron", time.Second)
		time.Sleep(time.Millisecond)
	}
	if delivery == nil || delivery.ExecutionID == "" || delivery.ID == "" || delivery.Attempt != 1 {
		t.Fatalf("delivery = %+v", delivery)
	}
	if err := server.Complete(context.Background(), sdkscheduler.CompleteRequest{
		ExecutionID: delivery.ExecutionID,
		LeaseToken:  delivery.LeaseToken,
		Status:      sdkscheduler.StatusSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleOnceCancelIdempotencyAndConflict(t *testing.T) {
	server, clock := newTestServer(t)
	once := sdkscheduler.Once{
		Namespace: "once", ID: "run", At: testNow.Add(time.Minute), Task: task("run"),
	}
	if err := server.ScheduleOnce(context.Background(), once); err != nil {
		t.Fatal(err)
	}
	if err := server.ScheduleOnce(context.Background(), once); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	conflict := once
	conflict.Task = task("different")
	if err := server.ScheduleOnce(context.Background(), conflict); !errdefs.IsConflict(err) {
		t.Fatalf("conflicting replay = %v", err)
	}
	if err := server.CancelOnce(context.Background(), "once", "run"); err != nil {
		t.Fatal(err)
	}
	if err := server.CancelOnce(context.Background(), "once", "run"); err != nil {
		t.Fatalf("cancel replay = %v", err)
	}
	clock.Advance(time.Minute)
	if delivery := claim(t, server, "once", time.Minute); delivery != nil {
		t.Fatalf("canceled delivery = %+v", delivery)
	}

	due := sdkscheduler.Once{
		Namespace: "once", ID: "due", At: clock.Now(), Task: task("due"),
	}
	if err := server.ScheduleOnce(context.Background(), due); err != nil {
		t.Fatal(err)
	}
	clock.Advance(0)
	delivery := claim(t, server, "once", time.Minute)
	if delivery == nil || delivery.ScheduleID != "due" {
		t.Fatalf("one-shot delivery = %+v", delivery)
	}
	if err := server.CancelOnce(context.Background(), "once", "due"); !errdefs.IsConflict(err) {
		t.Fatalf("cancel fired one-shot = %v", err)
	}
}

func TestOneShotRetentionReapsFiredAndCanceledWhileIdle(t *testing.T) {
	clock := newFakeClock()
	server, err := NewLocalServer(WithClock(clock), WithOnceRetention(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	fired := sdkscheduler.Once{
		Namespace: "retention", ID: "fired", At: testNow, Task: task("fired"),
	}
	if err := server.ScheduleOnce(context.Background(), fired); err != nil {
		t.Fatal(err)
	}
	clock.Advance(0)
	if err := server.ScheduleOnce(context.Background(), fired); err != nil {
		t.Fatalf("fired replay during retention: %v", err)
	}

	canceled := sdkscheduler.Once{
		Namespace: "retention", ID: "canceled", At: testNow.Add(time.Hour), Task: task("canceled"),
	}
	if err := server.ScheduleOnce(context.Background(), canceled); err != nil {
		t.Fatal(err)
	}
	if err := server.CancelOnce(context.Background(), "retention", "canceled"); err != nil {
		t.Fatal(err)
	}
	if err := server.CancelOnce(context.Background(), "retention", "canceled"); err != nil {
		t.Fatalf("cancel replay during retention: %v", err)
	}

	clock.Advance(time.Hour)
	server.mu.Lock()
	remaining := len(server.once)
	server.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("retained one-shots after idle expiry = %d", remaining)
	}
	if err := server.CancelOnce(context.Background(), "retention", "canceled"); !errdefs.IsNotFound(err) {
		t.Fatalf("cancel after retention = %v", err)
	}
	replacement := fired
	replacement.Task = task("replacement")
	if err := server.ScheduleOnce(context.Background(), replacement); err != nil {
		t.Fatalf("reuse ID after retention: %v", err)
	}
}

func TestCloseStopsOneShotRetentionTimer(t *testing.T) {
	clock := newFakeClock()
	server, err := NewLocalServer(WithClock(clock), WithOnceRetention(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	once := sdkscheduler.Once{
		Namespace: "close-retention", ID: "done", At: testNow, Task: task("done"),
	}
	if err := server.ScheduleOnce(context.Background(), once); err != nil {
		t.Fatal(err)
	}
	clock.Advance(0)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if len(clock.timers) != 2 || !clock.timers[1].stopped {
		t.Fatalf("retention timer not stopped on Close: %+v", clock.timers)
	}
}

func TestRuleUpsertReplayValidationAndClassification(t *testing.T) {
	server, _ := newTestServer(t)
	input := rule("rules", "job", sdkscheduler.OverlapSkip)
	input.Timezone = ""
	if err := server.PutRule(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := server.PutRule(context.Background(), input); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	listed, err := server.ListRules(context.Background(), "rules")
	if err != nil || len(listed) != 1 || listed[0].Timezone != "UTC" {
		t.Fatalf("ListRules = (%+v, %v)", listed, err)
	}
	replacement := input
	replacement.Cron = "@daily"
	if err := server.PutRule(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	bad := replacement
	bad.ID = "bad"
	bad.Cron = "* * * * * *"
	if err := server.PutRule(context.Background(), bad); !errdefs.IsValidation(err) {
		t.Fatalf("six-field cron = %v", err)
	}
	if err := server.DeleteRule(context.Background(), "rules", "missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("delete missing = %v", err)
	}
	if err := server.CancelOnce(context.Background(), "rules", "missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("cancel missing = %v", err)
	}
	if _, err := server.ListRules(context.Background(), ""); !errdefs.IsValidation(err) {
		t.Fatalf("list empty namespace = %v", err)
	}
}

func TestOverlapSkipAndAllow(t *testing.T) {
	server, _ := newTestServer(t)
	for _, overlap := range []sdkscheduler.Overlap{
		sdkscheduler.OverlapSkip, sdkscheduler.OverlapAllow,
	} {
		id := string(overlap)
		if id == "" {
			id = "skip"
		}
		if err := server.PutRule(context.Background(), rule("overlap", id, overlap)); err != nil {
			t.Fatal(err)
		}
		fireRuleForTest(t, server, "overlap", id)
		fireRuleForTest(t, server, "overlap", id)
		first := claim(t, server, "overlap", time.Minute)
		if first == nil || first.ScheduleID != id {
			t.Fatalf("%s first = %+v", id, first)
		}
		second := claim(t, server, "overlap", time.Minute)
		if overlap == sdkscheduler.OverlapSkip && second != nil {
			t.Fatalf("skip second = %+v", second)
		}
		if overlap == sdkscheduler.OverlapAllow && (second == nil || second.ScheduleID != id) {
			t.Fatalf("allow second = %+v", second)
		}
	}
}

func TestLeaseExpiryReclaimAndStaleOperations(t *testing.T) {
	server, clock := newTestServer(t)
	if err := server.PutRule(context.Background(), rule("lease", "job", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}
	fireRuleForTest(t, server, "lease", "job")
	first := claim(t, server, "lease", time.Minute)
	clock.Advance(time.Minute)
	second := claim(t, server, "lease", time.Minute)
	if second == nil || second.ExecutionID != first.ExecutionID ||
		second.ID != first.ID || second.Attempt != 2 || second.LeaseToken == first.LeaseToken {
		t.Fatalf("reclaimed delivery: first=%+v second=%+v", first, second)
	}
	if err := server.Renew(context.Background(), sdkscheduler.RenewRequest{
		ExecutionID: first.ExecutionID, LeaseToken: first.LeaseToken, LeaseDuration: time.Minute,
	}); !errdefs.IsConflict(err) {
		t.Fatalf("stale renew = %v", err)
	}
	if err := server.Complete(context.Background(), sdkscheduler.CompleteRequest{
		ExecutionID: first.ExecutionID, LeaseToken: first.LeaseToken,
		Status: sdkscheduler.StatusSucceeded,
	}); !errdefs.IsConflict(err) {
		t.Fatalf("stale complete = %v", err)
	}
	complete := sdkscheduler.CompleteRequest{
		ExecutionID: second.ExecutionID, LeaseToken: second.LeaseToken,
		Status: sdkscheduler.StatusFailed, Error: "failed",
	}
	if err := server.Complete(context.Background(), complete); err != nil {
		t.Fatal(err)
	}
	if err := server.Complete(context.Background(), complete); err != nil {
		t.Fatalf("identical complete replay = %v", err)
	}
	for _, different := range []sdkscheduler.CompleteRequest{
		{
			ExecutionID: complete.ExecutionID, LeaseToken: "different",
			Status: complete.Status, Error: complete.Error,
		},
		{
			ExecutionID: complete.ExecutionID, LeaseToken: complete.LeaseToken,
			Status: sdkscheduler.StatusSucceeded, Error: complete.Error,
		},
		{
			ExecutionID: complete.ExecutionID, LeaseToken: complete.LeaseToken,
			Status: complete.Status, Error: "different",
		},
	} {
		if err := server.Complete(context.Background(), different); !errdefs.IsConflict(err) {
			t.Fatalf("different complete replay %+v = %v", different, err)
		}
	}
}

func TestCompleteHistoryRetainsAllReplaysUntilExpiry(t *testing.T) {
	clock := newFakeClock()
	server, err := NewLocalServer(WithClock(clock), WithCompletionRetention(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.PutRule(context.Background(), rule("history", "job", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}

	var completed []sdkscheduler.CompleteRequest
	for range 3 {
		fireRuleForTest(t, server, "history", "job")
		delivery := claim(t, server, "history", time.Minute)
		request := sdkscheduler.CompleteRequest{
			ExecutionID: delivery.ExecutionID,
			LeaseToken:  delivery.LeaseToken,
			Status:      sdkscheduler.StatusSucceeded,
		}
		if err := server.Complete(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		completed = append(completed, request)
	}

	server.mu.Lock()
	executions, tombstones := len(server.executions), len(server.completions)
	server.mu.Unlock()
	if executions != 0 || tombstones != 3 {
		t.Fatalf("history sizes = executions %d, tombstones %d", executions, tombstones)
	}
	clock.Advance(59 * time.Minute)
	for _, request := range completed {
		if err := server.Complete(context.Background(), request); err != nil {
			t.Fatalf("retained replay = %v", err)
		}
	}
	clock.Advance(2 * time.Minute)
	if err := server.Complete(context.Background(), completed[0]); !errdefs.IsNotFound(err) {
		t.Fatalf("expired replay = %v", err)
	}
	server.mu.Lock()
	tombstones = len(server.completions)
	server.mu.Unlock()
	if tombstones != 0 {
		t.Fatalf("expired tombstones = %d", tombstones)
	}
}

func TestCompleteRetentionHonorsWorkerDeadlineAndExpires(t *testing.T) {
	clock := newFakeClock()
	server, err := NewLocalServer(
		WithClock(clock),
		WithCompletionRetention(time.Hour),
		WithMaxCompletionRetention(3*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.PutRule(context.Background(), rule("retained", "job", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}
	fireRuleForTest(t, server, "retained", "job")
	delivery := claim(t, server, "retained", time.Minute)
	retainUntil := clock.Now().Add(2 * time.Hour)
	request := sdkscheduler.CompleteRequest{
		ExecutionID: delivery.ExecutionID,
		LeaseToken:  delivery.LeaseToken,
		Status:      sdkscheduler.StatusSucceeded,
		RetainUntil: &retainUntil,
	}
	if err := server.Complete(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	clock.Advance(90 * time.Minute)
	if err := server.Complete(context.Background(), request); err != nil {
		t.Fatalf("replay inside worker deadline: %v", err)
	}
	clock.Advance(31 * time.Minute)
	if err := server.Complete(context.Background(), request); !errdefs.IsNotFound(err) {
		t.Fatalf("replay after worker deadline = %v", err)
	}
}

func TestCompleteRetentionRejectsDeadlineBeyondServerMaximum(t *testing.T) {
	clock := newFakeClock()
	server, err := NewLocalServer(
		WithClock(clock),
		WithCompletionRetention(time.Hour),
		WithMaxCompletionRetention(2*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.PutRule(context.Background(), rule("bounded", "job", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}
	fireRuleForTest(t, server, "bounded", "job")
	delivery := claim(t, server, "bounded", time.Minute)
	retainUntil := clock.Now().Add(2*time.Hour + time.Nanosecond)
	err = server.Complete(context.Background(), sdkscheduler.CompleteRequest{
		ExecutionID: delivery.ExecutionID,
		LeaseToken:  delivery.LeaseToken,
		Status:      sdkscheduler.StatusSucceeded,
		RetainUntil: &retainUntil,
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("oversized RetainUntil = %v", err)
	}
}

func TestRuleReplaceAndRemoveShareTriggerGate(t *testing.T) {
	t.Run("replace drops admitted stale fire", func(t *testing.T) {
		store := &blockingRuleStore{
			memoryRuleStore: &memoryRuleStore{rules: make(map[scheduleKey]sdkscheduler.Rule)},
		}
		server, err := NewLocalServer(WithRuleStore(store))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		key := scheduleKey{namespace: "race", id: "job"}
		if err := server.PutRule(context.Background(), rule("race", "job", sdkscheduler.OverlapAllow)); err != nil {
			t.Fatal(err)
		}
		server.mu.Lock()
		old := server.rules[key]
		server.mu.Unlock()

		store.saveEntered = make(chan struct{})
		store.continueSave = make(chan struct{})
		replaced := rule("race", "job", sdkscheduler.OverlapAllow)
		replaced.Task = task("new")
		replaceDone := make(chan error, 1)
		go func() { replaceDone <- server.PutRule(context.Background(), replaced) }()
		<-store.saveEntered

		fired := make(chan struct{})
		go func() {
			server.fireRule(key, old, old.generation)
			close(fired)
		}()
		if delivery := claim(t, server, "race", time.Minute); delivery != nil {
			t.Fatalf("fire passed trigger gate before replacement completed: %+v", delivery)
		}
		close(store.continueSave)
		if err := <-replaceDone; err != nil {
			t.Fatal(err)
		}
		<-fired
		if delivery := claim(t, server, "race", time.Minute); delivery != nil {
			t.Fatalf("replaced generation queued stale fire: %+v", delivery)
		}
	})

	t.Run("delete leaves queued execution", func(t *testing.T) {
		server, _ := newTestServer(t)
		key := scheduleKey{namespace: "race", id: "job"}
		if err := server.PutRule(context.Background(), rule("race", "job", sdkscheduler.OverlapAllow)); err != nil {
			t.Fatal(err)
		}
		server.mu.Lock()
		old := server.rules[key]
		server.mu.Unlock()
		server.fireRule(key, old, old.generation)
		if err := server.DeleteRule(context.Background(), "race", "job"); err != nil {
			t.Fatal(err)
		}
		server.fireRule(key, old, old.generation)
		if delivery := claim(t, server, "race", time.Minute); delivery == nil {
			t.Fatal("delete canceled an execution already queued before removal")
		}
		if extra := claim(t, server, "race", time.Minute); extra != nil {
			t.Fatalf("removed generation queued extra work: %+v", extra)
		}
	})
}

type memoryRuleStore struct {
	mu        sync.Mutex
	rules     map[scheduleKey]sdkscheduler.Rule
	listCalls int
}

type blockingRuleStore struct {
	*memoryRuleStore
	saveEntered          chan struct{}
	continueSave         chan struct{}
	deleteEntered        chan struct{}
	continueDelete       chan struct{}
	saveErr              error
	deleteErr            error
	waitSaveForContext   bool
	waitDeleteForContext bool
}

func (s *blockingRuleStore) Save(ctx context.Context, rule sdkscheduler.Rule) error {
	if s.saveEntered != nil {
		close(s.saveEntered)
		<-s.continueSave
		s.saveEntered = nil
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.waitSaveForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.memoryRuleStore.Save(ctx, rule)
}

func (s *blockingRuleStore) Delete(ctx context.Context, namespace, id string) error {
	if s.deleteEntered != nil {
		close(s.deleteEntered)
		<-s.continueDelete
		s.deleteEntered = nil
	}
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.waitDeleteForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.memoryRuleStore.Delete(ctx, namespace, id)
}

func TestPutRuleJoinsPersistenceRollbackTimeout(t *testing.T) {
	store := &blockingRuleStore{
		memoryRuleStore:      &memoryRuleStore{rules: make(map[scheduleKey]sdkscheduler.Rule)},
		saveEntered:          make(chan struct{}),
		continueSave:         make(chan struct{}),
		waitDeleteForContext: true,
	}
	server, err := NewLocalServer(WithRuleStore(store), WithRollbackTimeout(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	putDone := make(chan error, 1)
	go func() {
		putDone <- server.PutRule(context.Background(), rule("rollback", "put", sdkscheduler.OverlapAllow))
	}()
	<-store.saveEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()
	waitForServerClosed(t, server)
	close(store.continueSave)

	err = <-putDone
	if !errdefs.IsNotAvailable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PutRule error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRuleJoinsPersistenceRestoreTimeout(t *testing.T) {
	store := &blockingRuleStore{
		memoryRuleStore: &memoryRuleStore{rules: make(map[scheduleKey]sdkscheduler.Rule)},
	}
	server, err := NewLocalServer(WithRuleStore(store), WithRollbackTimeout(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.PutRule(context.Background(), rule("rollback", "delete", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}
	store.deleteEntered = make(chan struct{})
	store.continueDelete = make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- server.DeleteRule(context.Background(), "rollback", "delete")
	}()
	<-store.deleteEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()
	waitForServerClosed(t, server)
	store.waitSaveForContext = true
	close(store.continueDelete)

	err = <-deleteDone
	if !errdefs.IsNotAvailable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteRule error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func (s *memoryRuleStore) Save(_ context.Context, rule sdkscheduler.Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[scheduleKey{namespace: rule.Namespace, id: rule.ID}] = cloneRule(rule)
	return nil
}

func (s *memoryRuleStore) Delete(_ context.Context, namespace, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, scheduleKey{namespace: namespace, id: id})
	return nil
}

func (s *memoryRuleStore) List(context.Context) ([]sdkscheduler.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	result := make([]sdkscheduler.Rule, 0, len(s.rules))
	for _, rule := range s.rules {
		result = append(result, cloneRule(rule))
	}
	return result, nil
}

func TestRestoreSkipsBadRulesAndStartDoesNotRepeat(t *testing.T) {
	store := &memoryRuleStore{rules: map[scheduleKey]sdkscheduler.Rule{
		{namespace: "restore", id: "good"}: rule("restore", "good", sdkscheduler.OverlapAllow),
		{namespace: "restore", id: "bad"}: {
			Namespace: "restore", ID: "bad", Cron: "bad", Timezone: "UTC", Task: task("bad"),
		},
	}}
	server, err := NewLocalServer(WithRuleStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	count, err := server.Restore(context.Background())
	if count != 1 || err == nil {
		t.Fatalf("Restore = (%d, %v)", count, err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start repeated restore error: %v", err)
	}
	store.mu.Lock()
	calls := store.listCalls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("store List calls = %d", calls)
	}
	listed, err := server.ListRules(context.Background(), "restore")
	if err != nil || len(listed) != 1 || listed[0].ID != "good" {
		t.Fatalf("restored rules = (%+v, %v)", listed, err)
	}
}

func TestCloseWaitsForAdmittedTriggerAndRejectsOperations(t *testing.T) {
	server, _ := newTestServer(t)
	if err := server.PutRule(context.Background(), rule("close", "job", sdkscheduler.OverlapAllow)); err != nil {
		t.Fatal(err)
	}
	key := scheduleKey{namespace: "close", id: "job"}
	server.mu.Lock()
	state := server.rules[key]
	server.mu.Unlock()
	gate := server.gateForKey(key)
	gate.Lock()
	fired := make(chan struct{})
	go func() {
		server.fireRule(key, state, state.generation)
		close(fired)
	}()
	time.Sleep(10 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		_ = server.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned before admitted callback")
	case <-time.After(20 * time.Millisecond):
	}
	gate.Unlock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish")
	}
	<-fired
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.PutRule(context.Background(), rule("close", "new", "")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("PutRule after Close = %v", err)
	}
	if _, err := server.Claim(context.Background(), sdkscheduler.ClaimRequest{
		Namespace: "close", LeaseDuration: time.Minute,
	}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Claim after Close = %v", err)
	}
}

func TestOptionsRejectTypedNil(t *testing.T) {
	var store *memoryRuleStore
	if _, err := NewLocalServer(WithRuleStore(store)); !errdefs.IsValidation(err) {
		t.Fatalf("typed nil store = %v", err)
	}
	if _, err := NewLocalServer(WithCompletionRetention(0)); !errdefs.IsValidation(err) {
		t.Fatalf("zero completion retention = %v", err)
	}
	if _, err := NewLocalServer(WithMaxCompletionRetention(0)); !errdefs.IsValidation(err) {
		t.Fatalf("zero maximum completion retention = %v", err)
	}
	if _, err := NewLocalServer(WithOnceRetention(0)); !errdefs.IsValidation(err) {
		t.Fatalf("zero one-shot retention = %v", err)
	}
	if _, err := NewLocalServer(WithRollbackTimeout(0)); !errdefs.IsValidation(err) {
		t.Fatalf("zero rollback timeout = %v", err)
	}
	if _, err := NewLocalServer(nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil option = %v", err)
	}
}

func TestMissingIDsUseFixedGateStripes(t *testing.T) {
	server, _ := newTestServer(t)
	seen := make(map[*sync.Mutex]struct{})
	for i := range 10_000 {
		id := fmt.Sprintf("missing-%d", i)
		err := server.DeleteRule(context.Background(), "gates", id)
		if !errdefs.IsNotFound(err) {
			t.Fatalf("DeleteRule(%q) = %v", id, err)
		}
		seen[server.gateForKey(scheduleKey{namespace: "gates", id: id})] = struct{}{}
	}
	if len(seen) > scheduleGateStripes {
		t.Fatalf("gate stripes used = %d, max %d", len(seen), scheduleGateStripes)
	}
}
