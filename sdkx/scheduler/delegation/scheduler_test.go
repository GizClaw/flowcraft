package scheduler_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	localscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
	delegationscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler/delegation"
)

type service struct {
	mu            sync.Mutex
	requests      []sdkdelegation.Request
	delegate      sdkdelegation.Response
	delegateErr   error
	statuses      []sdkdelegation.Response
	getErr        error
	getCalls      int
	getEntered    chan struct{}
	blockGet      bool
	closeCalls    atomic.Int32
	requestNotify chan struct{}
}

func (s *service) Delegate(_ context.Context, request sdkdelegation.Request) (sdkdelegation.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	response, err := s.delegate, s.delegateErr
	notify := s.requestNotify
	s.mu.Unlock()
	if notify != nil {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	return response, err
}

func (s *service) Get(ctx context.Context, id string) (sdkdelegation.Response, error) {
	s.mu.Lock()
	s.getCalls++
	entered, block := s.getEntered, s.blockGet
	if s.getErr != nil {
		err := s.getErr
		s.mu.Unlock()
		return sdkdelegation.Response{}, err
	}
	var response sdkdelegation.Response
	if len(s.statuses) > 0 {
		response = s.statuses[0]
		if len(s.statuses) > 1 {
			s.statuses = s.statuses[1:]
		}
	}
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return sdkdelegation.Response{}, ctx.Err()
	}
	response.ID = id
	return response, nil
}

func (s *service) Close() error {
	s.closeCalls.Add(1)
	return nil
}

func (s *service) snapshot() ([]sdkdelegation.Request, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sdkdelegation.Request(nil), s.requests...), s.getCalls
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
	service sdkdelegation.Service,
	opts ...delegationscheduler.Option,
) *delegationscheduler.Scheduler {
	t.Helper()
	opts = append(opts,
		delegationscheduler.WithWorkerOptions(sdkscheduler.WithPollInterval(time.Millisecond)),
		delegationscheduler.WithDelegationPollInterval(time.Millisecond),
	)
	scheduler, err := delegationscheduler.New(t.Context(), server, "delegation", service, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Close() })
	return scheduler
}

func request() sdkdelegation.Request {
	return sdkdelegation.Request{
		Mode:   sdkdelegation.ModeAsync,
		Target: "writer",
		Input:  "write",
		Metadata: map[string]string{
			"tenant": "acme",
		},
	}
}

func terminalResponse(status sdkdelegation.Status) sdkdelegation.Response {
	response := sdkdelegation.Response{ID: "delegation-1", Status: status}
	if status == sdkdelegation.StatusFailed {
		response.Error = "backend unavailable"
	}
	if status == sdkdelegation.StatusCanceled {
		response.Error = "request canceled by operator"
	}
	return response
}

func start(t *testing.T, scheduler *delegationscheduler.Scheduler, server *localscheduler.LocalServer) {
	t.Helper()
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
}

func waitRequests(t *testing.T, service *service, count int, timeout time.Duration) []sdkdelegation.Request {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		requests, _ := service.snapshot()
		if len(requests) >= count {
			return requests
		}
		if time.Now().After(deadline) {
			t.Fatalf("delegations = %d, want at least %d", len(requests), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func receiveCompletion(t *testing.T, completed <-chan sdkscheduler.CompleteRequest) sdkscheduler.CompleteRequest {
	t.Helper()
	select {
	case completion := <-completed:
		return completion
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler execution was not completed")
		return sdkscheduler.CompleteRequest{}
	}
}

func TestRuleAndOneShotTriggerThroughTypedClient(t *testing.T) {
	server := newLocalServer(t)
	delegationService := &service{
		delegate:      terminalResponse(sdkdelegation.StatusSucceeded),
		requestNotify: make(chan struct{}, 4),
	}
	scheduler := newScheduler(t, server, delegationService)
	rules, err := scheduler.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Fatalf("New registered default rules: %+v", rules)
	}
	rule, err := scheduler.PutRule(t.Context(), delegationscheduler.Rule{
		ID:       "digest",
		Cron:     "@every 1s",
		Timezone: "UTC",
		Task:     request(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Namespace != "delegation" || rule.ID != "digest" {
		t.Fatalf("rule = %+v", rule)
	}
	rules, err = scheduler.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Task.Target != "writer" {
		t.Fatalf("rules = %+v", rules)
	}

	start(t, scheduler, server)
	once, err := scheduler.After(t.Context(), 0, request())
	if err != nil {
		t.Fatal(err)
	}
	requests := waitRequests(t, delegationService, 2, 3*time.Second)
	schedules := make(map[string]bool, len(requests))
	for _, request := range requests {
		schedules[request.Metadata[sdkscheduler.MetaScheduleID]] = true
	}
	if !schedules[rule.ID] || !schedules[once.ID] {
		t.Fatalf("triggered schedule IDs = %v, want %q and %q", schedules, rule.ID, once.ID)
	}
}

func TestCancelPreventsOneShotDelivery(t *testing.T) {
	server := newLocalServer(t)
	delegationService := &service{delegate: terminalResponse(sdkdelegation.StatusSucceeded)}
	scheduler := newScheduler(t, server, delegationService)
	start(t, scheduler, server)
	once, err := scheduler.After(t.Context(), time.Hour, request())
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Cancel(t.Context(), once.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if requests, _ := delegationService.snapshot(); len(requests) != 0 {
		t.Fatalf("canceled one-shot delivered %d requests", len(requests))
	}
}

func TestWorkerClonesMetadataAndOverwritesReservedKeys(t *testing.T) {
	local := newLocalServer(t)
	server := &observingServer{LocalServer: local, completed: make(chan sdkscheduler.CompleteRequest, 1)}
	delegationService := &service{delegate: terminalResponse(sdkdelegation.StatusSucceeded)}
	scheduler := newScheduler(t, server, delegationService)
	start(t, scheduler, local)

	input := request()
	input.Metadata[sdkscheduler.MetaScheduleID] = "wrong-schedule"
	input.Metadata[sdkscheduler.MetaDeliveryID] = "wrong-delivery"
	input.Metadata[sdkscheduler.MetaExecutionID] = "wrong-execution"
	once, err := scheduler.After(t.Context(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	completion := receiveCompletion(t, server.completed)
	requests := waitRequests(t, delegationService, 1, time.Second)
	got := requests[0]
	if got.Metadata["tenant"] != "acme" {
		t.Fatalf("tenant metadata = %q", got.Metadata["tenant"])
	}
	if got.Metadata[sdkscheduler.MetaScheduleID] != once.ID {
		t.Fatalf("schedule ID = %q, want %q", got.Metadata[sdkscheduler.MetaScheduleID], once.ID)
	}
	if got.Metadata[sdkscheduler.MetaDeliveryID] == "" ||
		got.Metadata[sdkscheduler.MetaDeliveryID] == "wrong-delivery" {
		t.Fatalf("delivery ID = %q", got.Metadata[sdkscheduler.MetaDeliveryID])
	}
	if got.IdempotencyKey != got.Metadata[sdkscheduler.MetaDeliveryID] {
		t.Fatalf(
			"idempotency key = %q, want delivery ID %q",
			got.IdempotencyKey,
			got.Metadata[sdkscheduler.MetaDeliveryID],
		)
	}
	if got.Metadata[sdkscheduler.MetaExecutionID] != completion.ExecutionID {
		t.Fatalf("execution ID = %q, want %q", got.Metadata[sdkscheduler.MetaExecutionID], completion.ExecutionID)
	}
	if input.Metadata[sdkscheduler.MetaScheduleID] != "wrong-schedule" ||
		input.Metadata[sdkscheduler.MetaDeliveryID] != "wrong-delivery" ||
		input.Metadata[sdkscheduler.MetaExecutionID] != "wrong-execution" {
		t.Fatal("worker mutated caller metadata")
	}
}

func TestAcceptedResponsePollsUntilTerminal(t *testing.T) {
	local := newLocalServer(t)
	server := &observingServer{LocalServer: local, completed: make(chan sdkscheduler.CompleteRequest, 1)}
	delegationService := &service{
		delegate: sdkdelegation.Response{ID: "delegation-1", Status: sdkdelegation.StatusAccepted},
		statuses: []sdkdelegation.Response{
			{Status: sdkdelegation.StatusRunning},
			{Status: sdkdelegation.StatusSucceeded},
		},
	}
	scheduler := newScheduler(t, server, delegationService)
	start(t, scheduler, local)
	if _, err := scheduler.After(t.Context(), 0, request()); err != nil {
		t.Fatal(err)
	}
	completion := receiveCompletion(t, server.completed)
	if completion.Status != sdkscheduler.StatusSucceeded || completion.Error != "" {
		t.Fatalf("completion = %+v", completion)
	}
	if _, calls := delegationService.snapshot(); calls != 2 {
		t.Fatalf("Get calls = %d, want 2", calls)
	}
}

func TestTerminalDelegationFailuresFailSchedulerExecution(t *testing.T) {
	for _, status := range []sdkdelegation.Status{
		sdkdelegation.StatusFailed,
		sdkdelegation.StatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			local := newLocalServer(t)
			server := &observingServer{
				LocalServer: local,
				completed:   make(chan sdkscheduler.CompleteRequest, 1),
			}
			delegationService := &service{delegate: terminalResponse(status)}
			scheduler := newScheduler(t, server, delegationService)
			start(t, scheduler, local)
			if _, err := scheduler.After(t.Context(), 0, request()); err != nil {
				t.Fatal(err)
			}
			completion := receiveCompletion(t, server.completed)
			if completion.Status != sdkscheduler.StatusFailed ||
				!strings.Contains(completion.Error, "delegation-1") {
				t.Fatalf("completion = %+v", completion)
			}
			if status == sdkdelegation.StatusFailed &&
				!strings.Contains(completion.Error, "backend unavailable") {
				t.Fatalf("failure lost business error: %+v", completion)
			}
		})
	}
}

func TestCloseCancelsBlockedGetWithoutClosingBorrowedDependencies(t *testing.T) {
	server := newLocalServer(t)
	delegationService := &service{
		delegate:   sdkdelegation.Response{ID: "delegation-1", Status: sdkdelegation.StatusAccepted},
		getEntered: make(chan struct{}, 1),
		blockGet:   true,
	}
	scheduler := newScheduler(t, server, delegationService)
	start(t, scheduler, server)
	if _, err := scheduler.After(t.Context(), 0, request()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delegationService.getEntered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter Get")
	}

	done := make(chan error, 1)
	go func() { done <- scheduler.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked Get")
	}
	if delegationService.closeCalls.Load() != 0 {
		t.Fatal("Close closed the borrowed delegation service")
	}
	if _, err := server.ListRules(t.Context(), "delegation"); err != nil {
		t.Fatalf("Close closed the borrowed scheduler server: %v", err)
	}
	if response, err := delegationService.Delegate(t.Context(), request()); err != nil ||
		response.ID != "delegation-1" {
		t.Fatalf("borrowed delegation service is unusable: response=%+v error=%v", response, err)
	}
}

func TestValidationAndLifecycleIdempotence(t *testing.T) {
	server := newLocalServer(t)
	delegationService := &service{delegate: terminalResponse(sdkdelegation.StatusSucceeded)}
	var nilServer *localscheduler.LocalServer
	var nilService *service
	tests := []struct {
		name string
		call func() error
	}{
		{"nil context", func() error {
			//nolint:staticcheck // deliberate: nil Context must be rejected
			_, err := delegationscheduler.New(nil, server, "delegation", delegationService)
			return err
		}},
		{"nil server", func() error {
			_, err := delegationscheduler.New(t.Context(), nil, "delegation", delegationService)
			return err
		}},
		{"typed nil server", func() error {
			_, err := delegationscheduler.New(t.Context(), nilServer, "delegation", delegationService)
			return err
		}},
		{"empty namespace", func() error {
			_, err := delegationscheduler.New(t.Context(), server, " ", delegationService)
			return err
		}},
		{"nil service", func() error {
			_, err := delegationscheduler.New(t.Context(), server, "delegation", nil)
			return err
		}},
		{"typed nil service", func() error {
			_, err := delegationscheduler.New(t.Context(), server, "delegation", nilService)
			return err
		}},
		{"nil option", func() error {
			_, err := delegationscheduler.New(t.Context(), server, "delegation", delegationService, nil)
			return err
		}},
		{"nil worker option", func() error {
			_, err := delegationscheduler.New(
				t.Context(), server, "delegation", delegationService,
				delegationscheduler.WithWorkerOptions(nil),
			)
			return err
		}},
		{"invalid delegation poll", func() error {
			_, err := delegationscheduler.New(
				t.Context(), server, "delegation", delegationService,
				delegationscheduler.WithDelegationPollInterval(0),
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

	scheduler := newScheduler(t, server, delegationService)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := scheduler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := scheduler.Start(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start after Close error = %v, want not available", err)
	}
}

func TestTaskAndClientMethodsValidateInputs(t *testing.T) {
	server := newLocalServer(t)
	scheduler := newScheduler(t, server, &service{
		delegate: terminalResponse(sdkdelegation.StatusSucceeded),
	})
	invalid := request()
	invalid.Target = ""
	if _, err := scheduler.PutRule(t.Context(), delegationscheduler.Rule{
		Cron: "@hourly",
		Task: invalid,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("PutRule error = %v, want validation", err)
	}
	if _, err := scheduler.After(t.Context(), 0, invalid); !errdefs.IsValidation(err) {
		t.Fatalf("After error = %v, want validation", err)
	}
	if err := scheduler.Cancel(t.Context(), ""); !errdefs.IsValidation(err) {
		t.Fatalf("Cancel error = %v, want validation", err)
	}
	if err := (delegationscheduler.Task{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("Task.Validate error = %v, want validation", err)
	}
}

func TestDelegateAndGetErrorsFailExecution(t *testing.T) {
	for _, test := range []struct {
		name    string
		service *service
		want    string
	}{
		{
			name:    "delegate",
			service: &service{delegateErr: errors.New("delegate unavailable")},
			want:    "delegate unavailable",
		},
		{
			name: "get",
			service: &service{
				delegate: sdkdelegation.Response{ID: "delegation-1", Status: sdkdelegation.StatusAccepted},
				getErr:   errors.New("status unavailable"),
			},
			want: "status unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := newLocalServer(t)
			server := &observingServer{
				LocalServer: local,
				completed:   make(chan sdkscheduler.CompleteRequest, 1),
			}
			scheduler := newScheduler(t, server, test.service)
			start(t, scheduler, local)
			if _, err := scheduler.After(t.Context(), 0, request()); err != nil {
				t.Fatal(err)
			}
			completion := receiveCompletion(t, server.completed)
			if completion.Status != sdkscheduler.StatusFailed ||
				!strings.Contains(completion.Error, test.want) {
				t.Fatalf("completion = %+v", completion)
			}
		})
	}
}
