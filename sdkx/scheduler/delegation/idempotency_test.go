package scheduler

import (
	"context"
	"maps"
	"strconv"
	"sync"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
)

type idempotentService struct {
	mu         sync.Mutex
	operations int
	requests   []sdkdelegation.Request
	responses  []sdkdelegation.Response
	cache      map[string]struct {
		request  sdkdelegation.Request
		response sdkdelegation.Response
	}
}

func (s *idempotentService) Delegate(
	_ context.Context,
	request sdkdelegation.Request,
) (sdkdelegation.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, cloneTestRequest(request))
	if cached, ok := s.cache[request.IdempotencyKey]; ok {
		if !sameTestRequest(cached.request, request) {
			panic("idempotency key reused for different request semantics")
		}
		s.responses = append(s.responses, cached.response)
		return cached.response, nil
	}
	s.operations++
	response := sdkdelegation.Response{
		ID:     "operation-" + strconv.Itoa(s.operations),
		Status: sdkdelegation.StatusSucceeded,
	}
	s.cache[request.IdempotencyKey] = struct {
		request  sdkdelegation.Request
		response sdkdelegation.Response
	}{request: cloneTestRequest(request), response: response}
	s.responses = append(s.responses, response)
	return response, nil
}

func (*idempotentService) Get(context.Context, string) (sdkdelegation.Response, error) {
	panic("terminal Delegate response must not be polled")
}

func cloneTestRequest(request sdkdelegation.Request) sdkdelegation.Request {
	request.Metadata = maps.Clone(request.Metadata)
	return request
}

func sameTestRequest(left, right sdkdelegation.Request) bool {
	return left.Mode == right.Mode &&
		left.Target == right.Target &&
		left.Input == right.Input &&
		left.IdempotencyKey == right.IdempotencyKey &&
		maps.Equal(left.Metadata, right.Metadata)
}

func TestHandlerUsesDeliveryIDForIdempotentRedelivery(t *testing.T) {
	service := &idempotentService{
		cache: make(map[string]struct {
			request  sdkdelegation.Request
			response sdkdelegation.Response
		}),
	}
	handler := &handler{service: service, pollInterval: time.Millisecond}
	request := Task{
		Mode:           sdkdelegation.ModeAsync,
		Target:         "writer",
		Input:          "write",
		IdempotencyKey: "caller-key-must-be-overwritten",
		Metadata:       map[string]string{"tenant": "acme"},
	}
	delivery := sdkscheduler.Delivery{
		ID:          "delivery-1",
		ScheduleID:  "schedule-1",
		ExecutionID: "execution-1",
	}

	if err := handler.Handle(t.Context(), delivery, request); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(t.Context(), delivery, request); err != nil {
		t.Fatal(err)
	}

	delivery.ID = "delivery-2"
	delivery.ExecutionID = "execution-2"
	if err := handler.Handle(t.Context(), delivery, request); err != nil {
		t.Fatal(err)
	}

	if service.operations != 2 {
		t.Fatalf("operations = %d, want 2 for two delivery IDs", service.operations)
	}
	if len(service.responses) != 3 ||
		service.responses[0].ID != service.responses[1].ID ||
		service.responses[2].ID == service.responses[0].ID {
		t.Fatalf("responses = %+v", service.responses)
	}
	if service.requests[0].IdempotencyKey != "delivery-1" ||
		service.requests[1].IdempotencyKey != "delivery-1" ||
		service.requests[2].IdempotencyKey != "delivery-2" {
		t.Fatalf("idempotency keys = %q, %q, %q",
			service.requests[0].IdempotencyKey,
			service.requests[1].IdempotencyKey,
			service.requests[2].IdempotencyKey,
		)
	}
	for _, got := range service.requests {
		if got.Metadata["tenant"] != "acme" {
			t.Fatalf("request lost metadata: %+v", got)
		}
	}
}
