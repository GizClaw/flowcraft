package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	corescheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
	delegationscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler/delegation"
)

type service struct {
	mu          sync.Mutex
	requests    []sdkdelegation.Request
	statuses    map[string]sdkdelegation.Response
	delegateErr error
	getErr      error
}

func newService() *service {
	return &service{statuses: make(map[string]sdkdelegation.Response)}
}

func (s *service) Delegate(_ context.Context, request sdkdelegation.Request) (sdkdelegation.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delegateErr != nil {
		return sdkdelegation.Response{}, s.delegateErr
	}
	id := "delegation-" + time.Now().Format("150405.000000000")
	s.requests = append(s.requests, request)
	response := sdkdelegation.Response{ID: id, Status: sdkdelegation.StatusAccepted}
	s.statuses[id] = response
	return response, nil
}

func (s *service) Get(_ context.Context, id string) (sdkdelegation.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return sdkdelegation.Response{}, s.getErr
	}
	return s.statuses[id], nil
}

func (s *service) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *service) request(index int) sdkdelegation.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[index]
}

func (s *service) finishAll(status sdkdelegation.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.statuses {
		s.statuses[id] = sdkdelegation.Response{ID: id, Status: status}
	}
}

func newScheduler(t *testing.T, service sdkdelegation.Service) *delegationscheduler.Scheduler {
	t.Helper()
	s, err := delegationscheduler.New(service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitCount(t *testing.T, service *service, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for service.count() < count {
		if time.Now().After(deadline) {
			t.Fatalf("delegations = %d, want at least %d", service.count(), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func request() sdkdelegation.Request {
	return sdkdelegation.Request{
		Mode:   sdkdelegation.ModeSync,
		Target: "writer",
		Input:  "write",
		Metadata: map[string]string{
			"tenant": "acme",
		},
	}
}

func TestAfterForcesAsyncAndAddsScheduleMetadata(t *testing.T) {
	service := newService()
	s := newScheduler(t, service)
	input := request()
	handle, err := s.After(context.Background(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	waitCount(t, service, 1)
	got := service.request(0)
	if got.Mode != sdkdelegation.ModeAsync {
		t.Fatalf("mode = %q, want async", got.Mode)
	}
	if got.Metadata[delegationscheduler.MetaScheduleID] != handle {
		t.Fatalf("schedule metadata = %q, want %q",
			got.Metadata[delegationscheduler.MetaScheduleID], handle)
	}
	if got.Metadata["tenant"] != "acme" || input.Metadata[delegationscheduler.MetaScheduleID] != "" {
		t.Fatal("metadata was not cloned and preserved")
	}
}

func TestAddValidatesRequestBeforeArming(t *testing.T) {
	s := newScheduler(t, newService())
	if _, err := s.Add(context.Background(), delegationscheduler.Rule{
		Cron: "@hourly",
		Value: sdkdelegation.Request{
			Mode:  sdkdelegation.ModeSync,
			Input: "missing target",
		},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("Add error = %v, want validation", err)
	}
}

func TestOverlapSkipUsesDelegationTerminalStatus(t *testing.T) {
	service := newService()
	s := newScheduler(t, service)
	id, err := s.Add(context.Background(), delegationscheduler.Rule{
		ID: "digest", Cron: "@every 5ms", Value: request(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	waitCount(t, service, 1)
	time.Sleep(1200 * time.Millisecond)
	if got := service.count(); got != 1 {
		t.Fatalf("delegations while accepted = %d, want 1", got)
	}
	if got := service.request(0).Metadata[corescheduler.MetaScheduleID]; got != id {
		t.Fatalf("schedule metadata = %q, want %q", got, id)
	}
	service.finishAll(sdkdelegation.StatusSucceeded)
	waitCount(t, service, 2)
}

func TestOverlapSkipTreatsRunningAsOutstanding(t *testing.T) {
	service := newService()
	s := newScheduler(t, service)
	if _, err := s.Add(context.Background(), delegationscheduler.Rule{
		Cron: "@every 5ms", Value: request(),
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	waitCount(t, service, 1)
	service.finishAll(sdkdelegation.StatusRunning)
	time.Sleep(1200 * time.Millisecond)
	if got := service.count(); got != 1 {
		t.Fatalf("delegations while running = %d, want 1", got)
	}
}

func TestGetErrorSkipsInsteadOfDuplicating(t *testing.T) {
	service := newService()
	s := newScheduler(t, service)
	if _, err := s.Add(context.Background(), delegationscheduler.Rule{
		Cron: "@every 5ms", Value: request(),
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	waitCount(t, service, 1)
	service.mu.Lock()
	service.getErr = errors.New("backend unavailable")
	service.mu.Unlock()
	time.Sleep(1200 * time.Millisecond)
	if got := service.count(); got != 1 {
		t.Fatalf("delegations after status error = %d, want 1", got)
	}
}

func TestNewRequiresServiceAndCloseIsIdempotent(t *testing.T) {
	if _, err := delegationscheduler.New(nil); !errdefs.IsValidation(err) {
		t.Fatalf("New(nil) error = %v", err)
	}
	s := newScheduler(t, newService())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
