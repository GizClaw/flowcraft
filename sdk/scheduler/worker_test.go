package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type workSourceStub struct {
	mu sync.Mutex

	deliveries []*Delivery
	claims     []ClaimRequest
	renews     []RenewRequest
	completes  []CompleteRequest

	renewErr       error
	completeErr    error
	claimErrors    []error
	renewErrors    []error
	completeErrors []error
	onRenew        func(context.Context, RenewRequest, int) error
	onComplete     func(CompleteRequest)
}

func (s *workSourceStub) Claim(ctx context.Context, request ClaimRequest) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = append(s.claims, request)
	if len(s.claimErrors) > 0 {
		err := s.claimErrors[0]
		s.claimErrors = s.claimErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(s.deliveries) == 0 {
		return nil, nil
	}
	delivery := s.deliveries[0]
	s.deliveries = s.deliveries[1:]
	return delivery, nil
}

func (s *workSourceStub) Renew(ctx context.Context, request RenewRequest) error {
	s.mu.Lock()
	s.renews = append(s.renews, request)
	call := len(s.renews)
	callback := s.onRenew
	if len(s.renewErrors) > 0 {
		err := s.renewErrors[0]
		s.renewErrors = s.renewErrors[1:]
		s.mu.Unlock()
		return err
	}
	err := s.renewErr
	s.mu.Unlock()
	if callback != nil {
		return callback(ctx, request, call)
	}
	return err
}

func (s *workSourceStub) Complete(_ context.Context, request CompleteRequest) error {
	s.mu.Lock()
	s.completes = append(s.completes, request)
	err := s.completeErr
	if len(s.completeErrors) > 0 {
		err = s.completeErrors[0]
		s.completeErrors = s.completeErrors[1:]
	}
	callback := s.onComplete
	s.mu.Unlock()
	if callback != nil && err == nil {
		callback(request)
	}
	return err
}

func (s *workSourceStub) snapshot() (claims []ClaimRequest, renews []RenewRequest, completes []CompleteRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ClaimRequest(nil), s.claims...),
		append([]RenewRequest(nil), s.renews...),
		append([]CompleteRequest(nil), s.completes...)
}

type workerJob struct {
	Value string `json:"value"`
}

func delivery(t *testing.T, id string) *Delivery {
	t.Helper()
	payload, err := NewJSONPayload("worker-job", 1, workerJob{Value: id})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return &Delivery{
		ID:          "delivery-" + id,
		ExecutionID: "execution-" + id,
		Namespace:   "jobs",
		ScheduleID:  "schedule-" + id,
		Task:        Task{Payload: payload},
		Attempt:     1,
		LeaseToken:  "lease-" + id,
		LeaseUntil:  now.Add(time.Second),
		ScheduledAt: now,
	}
}

func newTestWorker(t *testing.T, source WorkSource, handler Handler[workerJob], options ...WorkerOption) *Worker[workerJob] {
	t.Helper()
	base := []WorkerOption{
		WithLeaseDuration(time.Second),
		WithRenewInterval(20 * time.Millisecond),
		WithPollInterval(time.Millisecond),
		WithShutdownTimeout(time.Second),
		WithRetryBackoff(time.Millisecond, 4*time.Millisecond),
	}
	worker, err := NewWorker(source, "jobs", "worker-job", 1, handler, append(base, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestWorkerCompletesSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handlerErr error
		want       Status
	}{
		{"success", nil, StatusSucceeded},
		{"failure", errors.New("business failed"), StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			source := &workSourceStub{deliveries: []*Delivery{delivery(t, tc.name)}}
			source.onComplete = func(CompleteRequest) { cancel() }
			worker := newTestWorker(t, source, HandlerFunc[workerJob](
				func(_ context.Context, got Delivery, job workerJob) error {
					if got.Namespace != "jobs" || job.Value != tc.name {
						t.Errorf("handler input = %+v/%+v", got, job)
					}
					return tc.handlerErr
				},
			))
			if err := worker.Run(ctx); err != nil {
				t.Fatal(err)
			}
			claims, _, completes := source.snapshot()
			if len(claims) == 0 || claims[0].Namespace != "jobs" || claims[0].LeaseDuration != time.Second {
				t.Fatalf("claims = %+v", claims)
			}
			if len(completes) != 1 || completes[0].Status != tc.want {
				t.Fatalf("completes = %+v, want %q", completes, tc.want)
			}
			if tc.handlerErr != nil && completes[0].Error == "" {
				t.Fatal("failed completion should carry handler error")
			}
		})
	}
}

func TestWorkerRenewsLongRunningLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{deliveries: []*Delivery{delivery(t, "renew")}}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error {
			time.Sleep(70 * time.Millisecond)
			return nil
		},
	))
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, renews, _ := source.snapshot()
	if len(renews) < 2 {
		t.Fatalf("renew count = %d, want at least 2", len(renews))
	}
	for _, renew := range renews {
		if renew.ExecutionID != "execution-renew" || renew.LeaseToken != "lease-renew" {
			t.Fatalf("renew identity = %+v", renew)
		}
	}
}

func TestWorkerSlowRenewUsesRequestStartForLocalLeaseDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{deliveries: []*Delivery{delivery(t, "slow-renew")}}
	source.onRenew = func(_ context.Context, _ RenewRequest, call int) error {
		switch call {
		case 1:
			time.Sleep(70 * time.Millisecond)
			return nil
		case 2:
			return errdefs.NotAvailablef("transient after slow renewal")
		default:
			return errdefs.Conflictf("unexpected extra renewal")
		}
	}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			<-ctx.Done()
			return ctx.Err()
		},
	),
		WithLeaseDuration(120*time.Millisecond),
		WithRenewInterval(20*time.Millisecond),
		WithRetryBackoff(70*time.Millisecond, 70*time.Millisecond),
	)

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run after conservative lease loss: %v", err)
	}
	_, renews, completes := source.snapshot()
	if len(renews) != 2 {
		t.Fatalf("renew count = %d, want 2 without retry beyond conservative deadline", len(renews))
	}
	if len(completes) != 1 || completes[0].Status != StatusCanceled {
		t.Fatalf("completes = %+v, want canceled after conservative lease loss", completes)
	}
}

func TestWorkerRetriesTransientClaimErrorsWithoutStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{
		claimErrors: []error{
			errdefs.NotAvailablef("temporarily unavailable"),
			errdefs.Timeoutf("response timeout"),
			errdefs.RateLimitf("slow down"),
			errdefs.Internalf("temporary internal"),
			errors.New("unclassified transport failure"),
		},
		deliveries: []*Delivery{delivery(t, "claim-recovered")},
	}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	), WithRetryBackoff(time.Millisecond, 4*time.Millisecond))

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run after transient Claim failures: %v", err)
	}
	claims, _, completes := source.snapshot()
	if len(claims) < 6 || len(completes) != 1 {
		t.Fatalf("calls after recovery = claims %d, completes %d", len(claims), len(completes))
	}
}

func TestWorkerRetriesTransientRenewBeforeLeaseDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	item := delivery(t, "renew-recovered")
	item.LeaseUntil = time.Now().Add(250 * time.Millisecond)
	source := &workSourceStub{
		deliveries: []*Delivery{item},
		renewErrors: []error{
			errdefs.NotAvailablef("temporarily unavailable"),
			errdefs.Timeoutf("response timeout"),
			errdefs.RateLimitf("slow down"),
			errdefs.Internalf("temporary internal"),
			errors.New("unclassified transport failure"),
			nil,
		},
	}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		},
	), WithRetryBackoff(time.Millisecond, 4*time.Millisecond))

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run after transient Renew failures: %v", err)
	}
	_, renews, completes := source.snapshot()
	if len(renews) < 6 || len(completes) != 1 || completes[0].Status != StatusSucceeded {
		t.Fatalf("recovery calls = renews %d, completes %+v", len(renews), completes)
	}
}

func TestWorkerLeaseLossCancelsOnlyCurrentDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := delivery(t, "lost")
	second := delivery(t, "healthy")
	source := &workSourceStub{
		deliveries: []*Delivery{first, second},
		renewErrors: []error{
			errdefs.Conflictf("lease lost"),
			nil,
		},
		completeErrors: []error{
			errdefs.Conflictf("expected stale completion"),
			nil,
		},
	}
	var completed atomic.Int32
	source.onComplete = func(CompleteRequest) {
		if completed.Add(1) == 1 {
			cancel()
		}
	}
	started := make(chan struct{}, 2)
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(60 * time.Millisecond):
				return nil
			}
		},
	), WithMaxConcurrency(2), WithRetryBackoff(time.Millisecond, 4*time.Millisecond))

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	<-started
	if err := <-done; err != nil {
		t.Fatalf("lease loss stopped worker: %v", err)
	}
	_, _, completes := source.snapshot()
	if len(completes) != 2 {
		t.Fatalf("completions = %+v, want both deliveries settled", completes)
	}
}

func TestWorkerPermanentClaimErrorStopsRun(t *testing.T) {
	permanent := errdefs.Validationf("bad protocol request")
	source := &workSourceStub{claimErrors: []error{permanent}}
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	))
	err := worker.Run(context.Background())
	if !errors.Is(err, permanent) || !errdefs.IsValidation(err) {
		t.Fatalf("Run error = %v, want permanent validation", err)
	}
}

func TestWorkerShutdownCancelsActiveDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{deliveries: []*Delivery{delivery(t, "cancel")}}
	started := make(chan struct{})
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	))
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, _, completes := source.snapshot()
	if len(completes) != 1 || completes[0].Status != StatusCanceled {
		t.Fatalf("completes = %+v, want canceled", completes)
	}
}

func TestWorkerHonorsMaximumConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{}
	for i := range 4 {
		source.deliveries = append(source.deliveries, delivery(t, string(rune('a'+i))))
	}
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	startedTwo := make(chan struct{})
	var startOnce sync.Once
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			if current == 2 {
				startOnce.Do(func() { close(startedTwo) })
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	), WithMaxConcurrency(2))
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-startedTwo:
	case <-time.After(time.Second):
		t.Fatal("two handlers did not run concurrently")
	}
	time.Sleep(30 * time.Millisecond)
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", got)
	}
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerStaleRenewCancelsOnlyDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stale := errdefs.Conflictf("stale lease")
	source := &workSourceStub{
		deliveries: []*Delivery{delivery(t, "stale")},
		renewErr:   stale,
	}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			<-ctx.Done()
			return ctx.Err()
		},
	))
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run error = %v, want delivery-local lease loss", err)
	}
}

func TestWorkerStrictDecodeCompletesFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bad := delivery(t, "wrong-kind")
	bad.Task.Payload.Kind = "other"
	source := &workSourceStub{deliveries: []*Delivery{bad}}
	source.onComplete = func(CompleteRequest) { cancel() }
	var handled atomic.Bool
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error {
			handled.Store(true)
			return nil
		},
	))
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if handled.Load() {
		t.Fatal("handler ran for mismatched payload kind")
	}
	_, _, completes := source.snapshot()
	if len(completes) != 1 || completes[0].Status != StatusFailed || completes[0].Error == "" {
		t.Fatalf("completes = %+v, want failed decode", completes)
	}
}

func TestWorkerStaleCompleteDoesNotStopRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{
		deliveries: []*Delivery{
			delivery(t, "complete-stale"),
			delivery(t, "complete-healthy"),
		},
		completeErrors: []error{
			errdefs.Conflictf("stale completion lease"),
			nil,
		},
	}
	source.onComplete = func(request CompleteRequest) {
		if request.ExecutionID == "execution-complete-healthy" {
			cancel()
		}
	}
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	), WithMaxConcurrency(1))
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("stale Complete stopped Run: %v", err)
	}
	claims, _, completes := source.snapshot()
	if len(claims) < 2 || len(completes) != 2 {
		t.Fatalf("calls = claims %d, completes %+v; want next delivery processed", len(claims), completes)
	}
	if completes[1].ExecutionID != "execution-complete-healthy" {
		t.Fatalf("second completion = %+v", completes[1])
	}
}

func TestNewWorkerRejectsTypedNilDependencies(t *testing.T) {
	handler := HandlerFunc[workerJob](func(context.Context, Delivery, workerJob) error { return nil })
	var source *workSourceStub
	if _, err := NewWorker(source, "ns", "kind", 1, handler); !errdefs.IsValidation(err) {
		t.Fatalf("typed nil WorkSource error = %v", err)
	}
	var nilHandler HandlerFunc[workerJob]
	if _, err := NewWorker(&workSourceStub{}, "ns", "kind", 1, nilHandler); !errdefs.IsValidation(err) {
		t.Fatalf("typed nil Handler error = %v", err)
	}
}

func TestWorkerRetriesTransientCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{
		deliveries: []*Delivery{delivery(t, "retry-complete")},
		completeErrors: []error{
			errdefs.NotAvailablef("temporarily unavailable"),
			errdefs.Timeoutf("response timeout"),
			errdefs.RateLimitf("slow down"),
			errdefs.Internalf("temporary internal"),
			errors.New("unclassified transport failure"),
			nil,
		},
	}
	source.onComplete = func(CompleteRequest) { cancel() }
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	))
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, completes := source.snapshot()
	if len(completes) != 6 {
		t.Fatalf("completion attempts = %d, want 6", len(completes))
	}
	for _, request := range completes[1:] {
		if request != completes[0] {
			t.Fatalf("completion retry changed request: first=%+v retry=%+v", completes[0], request)
		}
	}
}

func TestWorkerAdvertisesCustomCompletionRetryWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{deliveries: []*Delivery{delivery(t, "retain-window")}}
	source.onComplete = func(CompleteRequest) { cancel() }
	before := time.Now().UTC()
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	), WithShutdownTimeout(2*time.Hour))
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, completes := source.snapshot()
	if len(completes) != 1 {
		t.Fatalf("completes = %+v", completes)
	}
	if completes[0].RetainUntil == nil ||
		completes[0].RetainUntil.Before(before.Add(2*time.Hour)) ||
		completes[0].RetainUntil.After(time.Now().UTC().Add(2*time.Hour)) {
		t.Fatalf("RetainUntil = %v, want custom two-hour retry window", completes[0].RetainUntil)
	}
}

func TestWorkerCompletionRetryStopsAtShutdownTimeout(t *testing.T) {
	source := &workSourceStub{
		deliveries:  []*Delivery{delivery(t, "retry-timeout")},
		completeErr: errdefs.NotAvailablef("still unavailable"),
	}
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(context.Context, Delivery, workerJob) error { return nil },
	), WithShutdownTimeout(40*time.Millisecond))
	err := worker.Run(context.Background())
	if !errdefs.IsTimeout(err) {
		t.Fatalf("Run error = %v, want timeout classification", err)
	}
	_, _, completes := source.snapshot()
	if len(completes) < 2 {
		t.Fatalf("completion attempts = %d, want retries", len(completes))
	}
}

func TestWorkerShutdownWaitsForCooperativeHandlerCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &workSourceStub{deliveries: []*Delivery{delivery(t, "cooperative")}}
	started := make(chan struct{})
	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	cleanupDone := make(chan struct{})
	worker := newTestWorker(t, source, HandlerFunc[workerJob](
		func(ctx context.Context, _ Delivery, _ workerJob) error {
			close(started)
			<-ctx.Done()
			close(cleanupStarted)
			<-allowCleanup
			close(cleanupDone)
			return ctx.Err()
		},
	), WithShutdownTimeout(50*time.Millisecond))

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	<-cleanupStarted
	select {
	case err := <-done:
		t.Fatalf("Run returned before handler cleanup: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	close(allowCleanup)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after handler cleanup")
	}
	select {
	case <-cleanupDone:
	default:
		t.Fatal("Run returned before cleanup completed")
	}
}
