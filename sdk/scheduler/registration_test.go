package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type registrationServer struct {
	mu sync.Mutex

	rules       []Rule
	putCalls    int
	putErrAt    int
	putErr      error
	putErrors   map[int]error
	putAfterErr map[int]error
	putHook     func(int) error
	deleteCalls []string
	deleteErr   map[string]error
	once        []Once
	canceled    []string

	claimCalls int
	claimErr   error
	delivery   *Delivery
	closeCalls int
}

type rollbackTimeoutServer struct {
	*registrationServer
	slowID string
}

func (s *rollbackTimeoutServer) DeleteRule(ctx context.Context, namespace, id string) error {
	if id == s.slowID {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.registrationServer.DeleteRule(ctx, namespace, id)
}

func (s *registrationServer) PutRule(_ context.Context, rule Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	if s.putHook != nil {
		if err := s.putHook(s.putCalls); err != nil {
			return err
		}
	}
	if err := s.putErrors[s.putCalls]; err != nil {
		return err
	}
	if s.putErrAt == s.putCalls {
		return s.putErr
	}
	for index := range s.rules {
		if s.rules[index].Namespace == rule.Namespace && s.rules[index].ID == rule.ID {
			s.rules[index] = rule
			return s.putAfterErr[s.putCalls]
		}
	}
	s.rules = append(s.rules, rule)
	return s.putAfterErr[s.putCalls]
}

func (s *registrationServer) DeleteRule(ctx context.Context, namespace, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.deleteCalls = append(s.deleteCalls, namespace+"/"+id)
	if err := s.deleteErr[id]; err != nil {
		return err
	}
	for index := range s.rules {
		if s.rules[index].Namespace == namespace && s.rules[index].ID == id {
			s.rules = append(s.rules[:index], s.rules[index+1:]...)
			break
		}
	}
	return nil
}

func (s *registrationServer) ListRules(_ context.Context, namespace string) ([]Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rules []Rule
	for _, rule := range s.rules {
		if rule.Namespace == namespace {
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

func (s *registrationServer) ScheduleOnce(_ context.Context, once Once) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.once = append(s.once, once)
	return nil
}

func (s *registrationServer) CancelOnce(_ context.Context, namespace, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = append(s.canceled, namespace+"/"+id)
	return nil
}

func (s *registrationServer) Claim(ctx context.Context, _ ClaimRequest) (*Delivery, error) {
	s.mu.Lock()
	s.claimCalls++
	err := s.claimErr
	delivery := s.delivery
	s.delivery = nil
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if delivery != nil {
		return delivery, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*registrationServer) Renew(context.Context, RenewRequest) error { return nil }

func (*registrationServer) Complete(context.Context, CompleteRequest) error { return nil }

func (s *registrationServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

func (s *registrationServer) snapshot() registrationServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return registrationServer{
		rules:       append([]Rule(nil), s.rules...),
		putCalls:    s.putCalls,
		deleteCalls: append([]string(nil), s.deleteCalls...),
		once:        append([]Once(nil), s.once...),
		canceled:    append([]string(nil), s.canceled...),
		claimCalls:  s.claimCalls,
		closeCalls:  s.closeCalls,
	}
}

type registrationTask struct {
	Name string `json:"name"`
}

func registrationSpec(rules ...TypedRule[registrationTask]) RegistrationSpec[registrationTask] {
	return RegistrationSpec[registrationTask]{
		Namespace:      "registration",
		PayloadKind:    "registration-task",
		PayloadVersion: 1,
		Rules:          rules,
		Handler: HandlerFunc[registrationTask](
			func(context.Context, Delivery, registrationTask) error { return nil },
		),
		WorkerOptions: []WorkerOption{
			WithLeaseDuration(time.Second),
			WithRenewInterval(100 * time.Millisecond),
			WithPollInterval(time.Millisecond),
		},
	}
}

func registrationWireRule(t *testing.T, id, name string) Rule {
	t.Helper()
	payload, err := NewJSONPayload(
		"registration-task",
		1,
		registrationTask{Name: name},
	)
	if err != nil {
		t.Fatal(err)
	}
	return Rule{
		Namespace: "registration",
		ID:        id,
		Cron:      "* * * * *",
		Task:      Task{Payload: payload},
	}
}

func registrationDelivery(t *testing.T) *Delivery {
	t.Helper()
	payload, err := NewJSONPayload(
		"registration-task",
		1,
		registrationTask{Name: "delivery"},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return &Delivery{
		ID:          "delivery",
		ExecutionID: "execution",
		Namespace:   "registration",
		ScheduleID:  "schedule",
		Task:        Task{Payload: payload},
		Attempt:     1,
		LeaseToken:  "lease",
		LeaseUntil:  now.Add(time.Second),
		ScheduledAt: now,
	}
}

func TestRegisterRollsBackRulesInReverseAndJoinsFailures(t *testing.T) {
	putErr := errors.New("put third")
	rollbackErr := errors.New("remove second")
	server := &registrationServer{
		putErrAt:  3,
		putErr:    putErr,
		deleteErr: map[string]error{"second": rollbackErr},
	}
	_, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{ID: "first", Cron: "* * * * *", Task: registrationTask{Name: "first"}},
		TypedRule[registrationTask]{ID: "second", Cron: "* * * * *", Task: registrationTask{Name: "second"}},
		TypedRule[registrationTask]{ID: "third", Cron: "* * * * *", Task: registrationTask{Name: "third"}},
	))
	if !errors.Is(err, putErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("Register error = %v, want put and rollback errors", err)
	}
	got := server.snapshot()
	wantDeletes := []string{"registration/third", "registration/second", "registration/first"}
	if len(got.deleteCalls) != len(wantDeletes) {
		t.Fatalf("delete calls = %v", got.deleteCalls)
	}
	for index, want := range wantDeletes {
		if got.deleteCalls[index] != want {
			t.Fatalf("delete calls = %v, want %v", got.deleteCalls, wantDeletes)
		}
	}
}

func TestRegisterRollsBackAmbiguousAppliedRule(t *testing.T) {
	timeoutErr := errdefs.Timeoutf("response lost after apply")
	for _, tc := range []struct {
		name     string
		id       string
		existing []Rule
		wantName string
	}{
		{name: "created rule is removed"},
		{
			name:     "replaced rule is restored",
			id:       "ambiguous",
			existing: []Rule{registrationWireRule(t, "ambiguous", "original")},
			wantName: "original",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &registrationServer{
				rules:       append([]Rule(nil), tc.existing...),
				putAfterErr: map[int]error{1: timeoutErr},
			}
			_, err := Register(context.Background(), server, registrationSpec(
				TypedRule[registrationTask]{
					ID: tc.id, Cron: "* * * * *",
					Task: registrationTask{Name: "replacement"},
				},
			))
			if !errors.Is(err, timeoutErr) {
				t.Fatalf("Register error = %v, want timeout", err)
			}
			got := server.snapshot()
			if tc.wantName == "" {
				if len(got.rules) != 0 {
					t.Fatalf("ambiguous new rule remained after rollback: %+v", got.rules)
				}
				return
			}
			if len(got.rules) != 1 {
				t.Fatalf("rules after rollback = %+v", got.rules)
			}
			task, decodeErr := DecodeJSON[registrationTask](
				got.rules[0].Task.Payload,
				"registration-task",
				1,
			)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if task.Name != tc.wantName {
				t.Fatalf("restored task = %q, want %q", task.Name, tc.wantName)
			}
		})
	}
}

func TestRegisterAmbiguousRemovalTreatsNotFoundAsRolledBack(t *testing.T) {
	timeoutErr := errdefs.Timeoutf("response lost after apply")
	server := &registrationServer{
		putAfterErr: map[int]error{1: timeoutErr},
		deleteErr:   map[string]error{"ambiguous": errdefs.NotFoundf("already absent")},
	}
	_, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "ambiguous", Cron: "* * * * *",
			Task: registrationTask{Name: "created"},
		},
	))
	if !errors.Is(err, timeoutErr) || errdefs.IsNotFound(err) {
		t.Fatalf("Register error = %v, want only original timeout", err)
	}
}

func TestRegisterAmbiguousApplyJoinsCompensationFailure(t *testing.T) {
	timeoutErr := errdefs.Timeoutf("response lost after apply")
	removeErr := errors.New("remove ambiguous rule")
	server := &registrationServer{
		putAfterErr: map[int]error{1: timeoutErr},
		deleteErr:   map[string]error{"ambiguous": removeErr},
	}
	_, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "ambiguous", Cron: "* * * * *",
			Task: registrationTask{Name: "created"},
		},
	))
	if !errors.Is(err, timeoutErr) || !errors.Is(err, removeErr) {
		t.Fatalf("Register error = %v, want timeout and compensation failure", err)
	}
}

func TestRegisterRollbackPreservesExistingRule(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existing    string
		replacement string
	}{
		{name: "identical", existing: "original", replacement: "original"},
		{name: "replaced", existing: "original", replacement: "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putErr := errors.New("later put failed")
			server := &registrationServer{
				rules:    []Rule{registrationWireRule(t, "existing", tc.existing)},
				putErrAt: 2,
				putErr:   putErr,
			}
			_, err := Register(context.Background(), server, registrationSpec(
				TypedRule[registrationTask]{
					ID: "existing", Cron: "* * * * *",
					Task: registrationTask{Name: tc.replacement},
				},
				TypedRule[registrationTask]{
					ID: "failure", Cron: "* * * * *",
					Task: registrationTask{Name: "failure"},
				},
			))
			if !errors.Is(err, putErr) {
				t.Fatalf("Register error = %v, want put failure", err)
			}
			got := server.snapshot()
			if len(got.rules) != 1 || got.rules[0].ID != "existing" {
				t.Fatalf("rules after rollback = %+v", got.rules)
			}
			task, decodeErr := DecodeJSON[registrationTask](
				got.rules[0].Task.Payload,
				"registration-task",
				1,
			)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if task.Name != tc.existing {
				t.Fatalf("restored task = %q, want %q", task.Name, tc.existing)
			}
			if !reflect.DeepEqual(got.deleteCalls, []string{"registration/failure"}) {
				t.Fatalf("rollback deletes = %v, want only ambiguous failed item", got.deleteCalls)
			}
		})
	}
}

func TestRegisterJoinsRestoreFailure(t *testing.T) {
	putErr := errors.New("later put failed")
	restoreErr := errors.New("restore failed")
	server := &registrationServer{
		rules: []Rule{registrationWireRule(t, "existing", "original")},
		putErrors: map[int]error{
			2: putErr,
			3: restoreErr,
		},
	}
	_, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "existing", Cron: "* * * * *",
			Task: registrationTask{Name: "replacement"},
		},
		TypedRule[registrationTask]{
			ID: "failure", Cron: "* * * * *",
			Task: registrationTask{Name: "failure"},
		},
	))
	if !errors.Is(err, putErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("Register error = %v, want put and restore failures", err)
	}
	if got := server.snapshot().deleteCalls; !reflect.DeepEqual(got, []string{"registration/failure"}) {
		t.Fatalf("rollback deletes = %v, want only ambiguous failed item", got)
	}
}

func TestRegisterRollbackOutlivesCanceledRegistrationContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := &registrationServer{}
	server.putHook = func(call int) error {
		if call != 2 {
			return nil
		}
		cancel()
		return ctx.Err()
	}
	_, err := Register(ctx, server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "created", Cron: "* * * * *",
			Task: registrationTask{Name: "created"},
		},
		TypedRule[registrationTask]{
			ID: "failure", Cron: "* * * * *",
			Task: registrationTask{Name: "failure"},
		},
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Register error = %v, want context cancellation", err)
	}
	got := server.snapshot()
	if len(got.rules) != 0 {
		t.Fatalf("rules after canceled rollback = %+v", got.rules)
	}
	wantDeletes := []string{"registration/failure", "registration/created"}
	if !reflect.DeepEqual(got.deleteCalls, wantDeletes) {
		t.Fatalf("rollback calls = %v", got.deleteCalls)
	}
}

func TestRollbackInstalledGivesEachRuleAnIndependentDeadline(t *testing.T) {
	base := &registrationServer{
		rules: []Rule{
			registrationWireRule(t, "fast", "fast"),
			registrationWireRule(t, "slow", "slow"),
		},
	}
	server := &rollbackTimeoutServer{registrationServer: base, slowID: "slow"}
	client, err := NewClient[registrationTask](
		server, "registration", "registration-task", 1)
	if err != nil {
		t.Fatal(err)
	}
	installed := []installedRule[registrationTask]{
		{id: "fast"},
		{id: "slow"},
	}

	failures := rollbackInstalledWithTimeout(
		context.Background(), client, installed, 25*time.Millisecond)
	if len(failures) != 1 || !errors.Is(failures[0], context.DeadlineExceeded) {
		t.Fatalf("rollback failures = %v, want only slow-rule deadline", failures)
	}
	got := base.snapshot()
	if !reflect.DeepEqual(got.deleteCalls, []string{"registration/fast"}) {
		t.Fatalf("rollback calls after slow timeout = %v, want fast rule attempted successfully", got.deleteCalls)
	}
	if len(got.rules) != 1 || got.rules[0].ID != "slow" {
		t.Fatalf("rules after rollback = %+v, want only timed-out slow rule", got.rules)
	}
}

func TestRegistrationRollbackRestoresInitialRulesConcurrentlyAndIdempotently(t *testing.T) {
	server := &registrationServer{
		rules: []Rule{registrationWireRule(t, "existing", "original")},
	}
	registration, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "existing", Cron: "* * * * *",
			Task: registrationTask{Name: "replacement"},
		},
		TypedRule[registrationTask]{
			ID: "created", Cron: "* * * * *",
			Task: registrationTask{Name: "created"},
		},
	))
	if err != nil {
		t.Fatal(err)
	}

	const callers = 20
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			errs <- registration.Rollback(context.Background())
		}()
	}
	wait.Wait()
	close(errs)
	for rollbackErr := range errs {
		if rollbackErr != nil {
			t.Fatalf("Rollback: %v", rollbackErr)
		}
	}
	if err := registration.Rollback(context.Background()); err != nil {
		t.Fatalf("repeated Rollback: %v", err)
	}
	if err := registration.Start(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start after Rollback error = %v, want not available", err)
	}

	got := server.snapshot()
	if len(got.rules) != 1 || got.rules[0].ID != "existing" {
		t.Fatalf("rules after rollback = %+v", got.rules)
	}
	task, err := DecodeJSON[registrationTask](
		got.rules[0].Task.Payload,
		"registration-task",
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "original" {
		t.Fatalf("restored task = %q, want original", task.Name)
	}
	if got.putCalls != 3 {
		t.Fatalf("PutRule calls = %d, want two installs and one restore", got.putCalls)
	}
	if len(got.deleteCalls) != 1 || got.deleteCalls[0] != "registration/created" {
		t.Fatalf("DeleteRule calls = %v, want one created-rule removal", got.deleteCalls)
	}
}

func TestRegistrationRollbackJoinsStopRestoreAndRemoveFailures(t *testing.T) {
	stopErr := errdefs.Conflictf("worker stopped")
	restoreErr := errors.New("restore failed")
	removeErr := errors.New("remove failed")
	server := &registrationServer{
		rules:     []Rule{registrationWireRule(t, "existing", "original")},
		claimErr:  stopErr,
		deleteErr: map[string]error{"created": removeErr},
	}
	registration, err := Register(context.Background(), server, registrationSpec(
		TypedRule[registrationTask]{
			ID: "existing", Cron: "* * * * *",
			Task: registrationTask{Name: "replacement"},
		},
		TypedRule[registrationTask]{
			ID: "created", Cron: "* * * * *",
			Task: registrationTask{Name: "created"},
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.putErrors = map[int]error{3: restoreErr}
	server.mu.Unlock()
	if err := registration.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if startErr := registration.Start(); errors.Is(startErr, stopErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not expose its terminal error")
		}
		time.Sleep(time.Millisecond)
	}

	err = registration.Rollback(context.Background())
	if !errors.Is(err, stopErr) || !errors.Is(err, restoreErr) || !errors.Is(err, removeErr) {
		t.Fatalf("Rollback error = %v, want stop, restore, and remove failures", err)
	}
	if repeated := registration.Rollback(context.Background()); repeated != err {
		t.Fatalf("repeated Rollback error = %p, want cached %p", repeated, err)
	}
}

func TestRegistrationStartCloseConcurrentAndBorrowedServer(t *testing.T) {
	server := &registrationServer{}
	registration, err := Register(context.Background(), server, registrationSpec())
	if err != nil {
		t.Fatal(err)
	}

	var startWG sync.WaitGroup
	for range 20 {
		startWG.Add(1)
		go func() {
			defer startWG.Done()
			if err := registration.Start(); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
	}
	startWG.Wait()
	deadline := time.Now().Add(time.Second)
	for server.snapshot().claimCalls == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not begin claiming")
		}
		time.Sleep(time.Millisecond)
	}

	var closeWG sync.WaitGroup
	var closeErrors atomic.Int32
	for range 20 {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			if err := registration.Close(); err != nil {
				closeErrors.Add(1)
			}
		}()
	}
	closeWG.Wait()
	if closeErrors.Load() != 0 {
		t.Fatalf("Close errors = %d", closeErrors.Load())
	}
	got := server.snapshot()
	if got.claimCalls != 1 {
		t.Fatalf("Claim calls = %d, want one worker", got.claimCalls)
	}
	if got.closeCalls != 0 {
		t.Fatalf("borrowed Server Close calls = %d", got.closeCalls)
	}
	if err := registration.Start(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start after Close error = %v, want not available", err)
	}
}

func TestRegistrationCloseWaitsForHandlerCleanup(t *testing.T) {
	server := &registrationServer{delivery: registrationDelivery(t)}
	started := make(chan struct{})
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	cleanupDone := make(chan struct{})
	spec := registrationSpec()
	spec.Handler = HandlerFunc[registrationTask](
		func(ctx context.Context, _ Delivery, _ registrationTask) error {
			close(started)
			<-ctx.Done()
			close(cleanupStarted)
			<-allowCleanup
			close(cleanupDone)
			return ctx.Err()
		},
	)
	registration, err := Register(context.Background(), server, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Start(); err != nil {
		t.Fatal(err)
	}
	<-started
	closed := make(chan error, 1)
	go func() { closed <- registration.Close() }()
	<-cleanupStarted
	select {
	case err := <-closed:
		t.Fatalf("Close returned before handler cleanup: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(allowCleanup)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after handler cleanup")
	}
	select {
	case <-cleanupDone:
	default:
		t.Fatal("Close returned before cleanup completed")
	}
}

func TestRegistrationStartRacesClose(t *testing.T) {
	server := &registrationServer{}
	registration, err := Register(context.Background(), server, registrationSpec())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	var unexpected atomic.Int32
	for index := range 40 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if index%2 == 0 {
				err := registration.Start()
				if err != nil && !errdefs.IsNotAvailable(err) {
					unexpected.Add(1)
				}
				return
			}
			if err := registration.Close(); err != nil {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Start/Close did not finish")
	}
	if unexpected.Load() != 0 {
		t.Fatalf("unexpected lifecycle errors = %d", unexpected.Load())
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	if got := server.snapshot().closeCalls; got != 0 {
		t.Fatalf("borrowed Server Close calls = %d", got)
	}
}

func TestRegistrationReturnsUnexpectedWorkerError(t *testing.T) {
	stale := errdefs.Conflictf("stale scheduler")
	server := &registrationServer{claimErr: stale}
	registration, err := Register(context.Background(), server, registrationSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = registration.Start()
		if errors.Is(err, stale) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Start never observed worker error; last error %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := registration.Close(); !errors.Is(err, stale) {
		t.Fatalf("Close error = %v, want stale worker error", err)
	}
}

func TestRegistrationForwardsClientOperations(t *testing.T) {
	server := &registrationServer{}
	registration, err := Register(context.Background(), server, registrationSpec())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rule, err := registration.PutRule(ctx, TypedRule[registrationTask]{
		ID: "rule", Cron: "* * * * *", Task: registrationTask{Name: "rule"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registration.After(ctx, time.Hour, registrationTask{Name: "after"}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour)
	if _, err := registration.At(ctx, at, registrationTask{Name: "at"}); err != nil {
		t.Fatal(err)
	}
	list, err := registration.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Task.Name != "rule" {
		t.Fatalf("List = %+v", list)
	}
	if err := registration.Cancel(ctx, "once"); err != nil {
		t.Fatal(err)
	}
	if err := registration.Remove(ctx, rule.ID); err != nil {
		t.Fatal(err)
	}
	got := server.snapshot()
	if len(got.once) != 2 || len(got.canceled) != 1 || len(got.rules) != 0 {
		t.Fatalf("forwarded state = once:%d canceled:%v rules:%d", len(got.once), got.canceled, len(got.rules))
	}
}

func TestRegisterValidatesSpec(t *testing.T) {
	server := &registrationServer{}
	cases := []RegistrationSpec[registrationTask]{
		{},
		{Namespace: "ns", PayloadKind: "kind", PayloadVersion: 1},
	}
	for _, spec := range cases {
		if _, err := Register(context.Background(), server, spec); !errdefs.IsValidation(err) {
			t.Fatalf("Register error = %v, want validation", err)
		}
	}
}
