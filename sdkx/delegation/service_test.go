package delegation

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

type testEngineFactory struct {
	engine agent.Engine
}

func (f testEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: "local-delegation-test"}
}

func (f testEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return f.engine, nil
}

func buildResult(t *testing.T, engine agent.Engine) *deploy.Result {
	t.Helper()
	registry := agent.NewRegistry()
	if err := registry.Register(testEngineFactory{engine: engine}); err != nil {
		t.Fatal(err)
	}
	document, err := deploy.Parse([]byte(`
version: v1
agents:
  writer:
    card: {name: Writer, description: Writes prose}
    engine: {kind: local-delegation-test}
  researcher:
    card: {name: Researcher, description: Finds facts}
    engine: {kind: local-delegation-test}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := deploy.NewBuilder(registry).Build(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	return result
}

func completedEngine(output string) agent.Engine {
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, output))
		return board, nil
	})
}

func boundDirectory(t *testing.T, engine agent.Engine) *Directory {
	t.Helper()
	directory := NewDirectory()
	if err := directory.Bind(buildResult(t, engine)); err != nil {
		t.Fatal(err)
	}
	return directory
}

func syncRequest(target string) sdkdelegation.Request {
	return sdkdelegation.Request{
		Mode:   sdkdelegation.ModeSync,
		Target: target,
		Input:  "do it",
	}
}

func TestDirectoryBindListGetLookup(t *testing.T) {
	directory := NewDirectory()
	if _, err := directory.List(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("unbound List error = %v, want not available", err)
	}
	if err := directory.Bind(nil); !errdefs.IsValidation(err) {
		t.Fatalf("Bind(nil) error = %v, want validation", err)
	}
	result := buildResult(t, completedEngine("ok"))
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	if err := directory.Bind(result); !errdefs.IsConflict(err) {
		t.Fatalf("second Bind error = %v, want conflict", err)
	}

	targets, err := directory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{targets[0].ID, targets[1].ID}; !reflect.DeepEqual(got, []string{"researcher", "writer"}) {
		t.Fatalf("target order = %v", got)
	}
	if targets[0].Description != "Finds facts" || targets[0].Metadata["name"] != "Researcher" {
		t.Fatalf("researcher target = %+v", targets[0])
	}
	targets[0].Metadata["name"] = "mutated"
	target, err := directory.Get(context.Background(), "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if target.Metadata["name"] != "Researcher" {
		t.Fatal("List did not return a defensive target copy")
	}
	instance, err := directory.Lookup(context.Background(), "writer")
	if err != nil || instance.Agent.Card.Name != "Writer" {
		t.Fatalf("Lookup(writer) = (%v, %v)", instance, err)
	}
	for _, lookup := range []func() error{
		func() error { _, err := directory.Get(context.Background(), "ghost"); return err },
		func() error { _, err := directory.Lookup(context.Background(), "ghost"); return err },
	} {
		err := lookup()
		if !errors.Is(err, sdkdelegation.ErrTargetNotFound) || !errdefs.IsNotFound(err) {
			t.Fatalf("unknown target error = %v", err)
		}
	}
}

func TestDirectoryLookupUsesTargetIDIndex(t *testing.T) {
	result := buildResult(t, completedEngine("ok"))
	instance, ok := result.Instance("writer")
	if !ok {
		t.Fatal("build result has no writer instance")
	}
	instance.Agent.ID = "author"

	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	got, err := directory.Lookup(context.Background(), "author")
	if err != nil {
		t.Fatal(err)
	}
	if got != instance {
		t.Fatal("Lookup did not return the indexed deploy instance")
	}
}

func TestServiceSyncAndHandoff(t *testing.T) {
	service, err := NewService(boundDirectory(t, completedEngine("finished")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != sdkdelegation.StatusSucceeded || response.Output != "finished" || response.ID == "" {
		t.Fatalf("sync response = %+v", response)
	}

	handoff := syncRequest("researcher")
	handoff.Mode = sdkdelegation.ModeHandoff
	response, err = service.Delegate(context.Background(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != sdkdelegation.StatusAccepted ||
		response.Metadata["control_transfer"] != "true" ||
		response.Metadata["target"] != "researcher" {
		t.Fatalf("handoff response = %+v", response)
	}
}

type fakeAsyncBackend struct {
	mu        sync.Mutex
	submitted []AsyncRequest
	id        string
	response  sdkdelegation.Response
	err       error
}

func (b *fakeAsyncBackend) Submit(_ context.Context, request AsyncRequest) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submitted = append(b.submitted, request)
	return b.id, b.err
}

func (b *fakeAsyncBackend) Status(context.Context, string) (sdkdelegation.Response, error) {
	return b.response, b.err
}

func TestServiceAsyncSubmitAndStatus(t *testing.T) {
	backend := &fakeAsyncBackend{
		id: "job-1",
		response: sdkdelegation.Response{
			ID:     "backend-specific-id",
			Status: sdkdelegation.StatusRunning,
			Output: "must be stripped",
		},
	}
	service, err := NewService(boundDirectory(t, completedEngine("unused")), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	request.Metadata = map[string]string{"tenant": "acme"}
	response, err := service.Delegate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "job-1" || response.Status != sdkdelegation.StatusAccepted {
		t.Fatalf("async response = %+v", response)
	}
	request.Metadata["tenant"] = "mutated"
	if len(backend.submitted) != 1 ||
		backend.submitted[0].Request.Metadata["tenant"] != "acme" ||
		backend.submitted[0].Depth != 1 {
		t.Fatalf("submitted = %+v", backend.submitted)
	}

	response, err = service.Get(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "job-1" || response.Status != sdkdelegation.StatusRunning ||
		response.Output != "" || response.Error != "" {
		t.Fatalf("normalized status = %+v", response)
	}
}

type queueBackend struct {
	work      chan Work
	completed chan sdkdelegation.Response
	closeCall atomic.Int32
	nextID    atomic.Int32
}

func newQueueBackend() *queueBackend {
	return &queueBackend{
		work:      make(chan Work, 4),
		completed: make(chan sdkdelegation.Response, 4),
	}
}

func (b *queueBackend) Submit(_ context.Context, request AsyncRequest) (string, error) {
	id := "queued-" + strconv.Itoa(int(b.nextID.Add(1)))
	b.work <- Work{ID: id, LeaseToken: "lease-" + id, Request: request}
	return id, nil
}

func (b *queueBackend) Status(context.Context, string) (sdkdelegation.Response, error) {
	return sdkdelegation.Response{Status: sdkdelegation.StatusRunning}, nil
}

func (b *queueBackend) Claim(ctx context.Context) (Work, error) {
	select {
	case work := <-b.work:
		return work, nil
	case <-ctx.Done():
		return Work{}, ctx.Err()
	}
}

func (b *queueBackend) Complete(
	_ context.Context,
	_, _ string,
	response sdkdelegation.Response,
) error {
	b.completed <- response
	return nil
}

func (b *queueBackend) Close() error {
	b.closeCall.Add(1)
	return nil
}

func TestServiceWorkerExecutesWorkWithoutOwningBackend(t *testing.T) {
	backend := newQueueBackend()
	service, err := NewService(
		boundDirectory(t, completedEngine("from worker")),
		backend,
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	accepted, err := service.Delegate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-backend.completed:
		if completed.ID != accepted.ID ||
			completed.Status != sdkdelegation.StatusSucceeded ||
			completed.Output != "from worker" {
			t.Fatalf("completed work = %+v", completed)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not complete submitted work")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if got := backend.closeCall.Load(); got != 0 {
		t.Fatalf("backend Close calls = %d, want 0", got)
	}
}

func TestServiceUnknownTargetAndUnsupportedAsync(t *testing.T) {
	service, err := NewService(boundDirectory(t, completedEngine("ok")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	if _, err := service.Delegate(context.Background(), syncRequest("ghost")); !errdefs.IsNotFound(err) {
		t.Fatalf("unknown target error = %v", err)
	}
	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	if _, err := service.Delegate(context.Background(), request); !errors.Is(err, sdkdelegation.ErrUnsupportedMode) {
		t.Fatalf("async without backend error = %v", err)
	}
}

type markedHost struct {
	agent.NoopHost
}

func TestServicePropagatesContextHost(t *testing.T) {
	want := &markedHost{}
	seen := make(chan agent.Host, 1)
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		seen <- host
		return board, nil
	})
	service, err := NewService(boundDirectory(t, engine), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	ctx := agent.ContextWithHost(context.Background(), want)
	if _, err := service.Delegate(ctx, syncRequest("writer")); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != want {
		t.Fatalf("engine host = %T %v, want context host", got, got)
	}
}

type eventBusMarkedHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventBusMarkedHost) EventBus() event.Bus { return h.bus }

func TestServiceWorkerHostPreservesCapabilities(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	baseHost := &eventBusMarkedHost{bus: bus}
	backend := newQueueBackend()
	seen := make(chan error, 1)
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if got, ok := agent.EventBusFromHost(host); !ok || got != bus {
			seen <- errors.New("worker host did not preserve the base EventBus")
			return board, nil
		}
		if _, ok := sdkdelegation.ServiceFromHost(host); !ok {
			seen <- errors.New("worker host has no delegation Service capability")
			return board, nil
		}
		seen <- nil
		return board, nil
	})
	service, err := NewService(
		boundDirectory(t, engine),
		backend,
		WithMaxConcurrency(1),
		WithWorkerHost(baseHost),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	if _, err := service.Delegate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-seen:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not execute")
	}
}

func TestServiceWorkerDefaultHostSupportsNestedDelegation(t *testing.T) {
	backend := newQueueBackend()
	var calls atomic.Int32
	nested := make(chan sdkdelegation.Response, 1)
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if calls.Add(1) != 1 {
			board.AppendChannelMessage(agent.MainChannel,
				inference.NewTextMessage(inference.RoleAssistant, "nested"))
			return board, nil
		}
		service, ok := sdkdelegation.ServiceFromHost(host)
		if !ok {
			return board, errors.New("worker host has no delegation service")
		}
		response, err := service.Delegate(
			agent.ContextWithHost(ctx, host),
			syncRequest("researcher"),
		)
		if err != nil {
			return board, err
		}
		nested <- response
		return board, nil
	})
	service, err := NewService(
		boundDirectory(t, engine),
		backend,
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	if _, err := service.Delegate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-nested:
		if response.Status != sdkdelegation.StatusSucceeded || response.Output != "nested" {
			t.Fatalf("nested response = %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("nested delegation did not finish")
	}
}

func TestServiceTimeout(t *testing.T) {
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		<-ctx.Done()
		return board, ctx.Err()
	})
	service, err := NewService(boundDirectory(t, engine), nil, WithTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != sdkdelegation.StatusCanceled ||
		!strings.Contains(response.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("timeout response = %+v", response)
	}
}

func TestServiceSelfDelegationHonorsDepth(t *testing.T) {
	var service *Service
	var calls atomic.Int32
	depthError := make(chan error, 1)
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		call := calls.Add(1)
		if call <= 3 {
			nestedCtx := agent.ContextWithHost(ctx, host)
			_, err := service.Delegate(nestedCtx, syncRequest("writer"))
			if err != nil {
				depthError <- err
			}
		}
		return board, nil
	})
	var err error
	service, err = NewService(
		boundDirectory(t, engine),
		nil,
		WithMaxDepth(2),
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-depthError:
		if !errdefs.IsPolicyDenied(err) {
			t.Fatalf("depth error = %v, want policy denied", err)
		}
	case <-time.After(time.Second):
		t.Fatal("self delegation did not hit depth limit")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestServiceConcurrencyLimit(t *testing.T) {
	var current atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		now := current.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		started <- struct{}{}
		<-release
		current.Add(-1)
		return board, nil
	})
	service, err := NewService(boundDirectory(t, engine), nil, WithMaxConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	var calls sync.WaitGroup
	for range 3 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			_, _ = service.Delegate(context.Background(), syncRequest("writer"))
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two executions did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("third execution started before a slot was released")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	calls.Wait()
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", got)
	}
}

func TestServiceCloseWaitsRejectsAndIsIdempotent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		close(started)
		<-release
		return board, nil
	})
	service, err := NewService(boundDirectory(t, engine), nil)
	if err != nil {
		t.Fatal(err)
	}
	delegated := make(chan struct{})
	go func() {
		defer close(delegated)
		_, _ = service.Delegate(context.Background(), syncRequest("writer"))
	}()
	<-started

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = service.Close()
	}()
	for {
		service.stateMu.Lock()
		isClosed := service.closed
		service.stateMu.Unlock()
		if isClosed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Delegate during Close error = %v, want not available", err)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before admitted execution finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-delegated
	<-closed
	if err := service.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestResponseFromAgentMapsInterruptedToCanceled(t *testing.T) {
	response := responseFromAgent(&agent.Result{
		RunID:  "interrupted-run",
		Status: agent.StatusInterrupted,
		Err:    errdefs.Interruptedf("operator stopped the run"),
	})
	if response.ID != "interrupted-run" ||
		response.Status != sdkdelegation.StatusCanceled ||
		!strings.Contains(response.Error, "operator stopped") {
		t.Fatalf("response = %+v", response)
	}
}

type workerTestBackend struct {
	claim    func(context.Context) (Work, error)
	complete func(context.Context, string, string, sdkdelegation.Response) error
}

func (b *workerTestBackend) Submit(context.Context, AsyncRequest) (string, error) {
	return "unused", nil
}

func (b *workerTestBackend) Status(context.Context, string) (sdkdelegation.Response, error) {
	return sdkdelegation.Response{Status: sdkdelegation.StatusRunning}, nil
}

func (b *workerTestBackend) Claim(ctx context.Context) (Work, error) {
	return b.claim(ctx)
}

func (b *workerTestBackend) Complete(
	ctx context.Context,
	id string,
	leaseToken string,
	response sdkdelegation.Response,
) error {
	return b.complete(ctx, id, leaseToken, response)
}

func asyncWork(id string) Work {
	request := syncRequest("writer")
	request.Mode = sdkdelegation.ModeAsync
	return Work{
		ID:         id,
		LeaseToken: "lease-" + id,
		Request: AsyncRequest{
			Request: request,
			Depth:   1,
		},
	}
}

func TestServiceWorkerClaimErrorsBackOffAndStop(t *testing.T) {
	var claims atomic.Int32
	backend := &workerTestBackend{
		claim: func(context.Context) (Work, error) {
			call := claims.Add(1)
			if call < 4 {
				return Work{}, errors.New("temporary claim failure")
			}
			return Work{}, errdefs.NotAvailablef("backend closed")
		},
		complete: func(context.Context, string, string, sdkdelegation.Response) error {
			t.Fatal("Complete called without work")
			return nil
		},
	}
	started := time.Now()
	service, err := NewService(
		boundDirectory(t, completedEngine("unused")),
		backend,
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for claims.Load() < 4 && time.Since(started) < time.Second {
		time.Sleep(time.Millisecond)
	}
	if got := claims.Load(); got != 4 {
		t.Fatalf("Claim calls = %d, want 4", got)
	}
	if elapsed := time.Since(started); elapsed < 25*time.Millisecond {
		t.Fatalf("claim retries busy-looped in %v", elapsed)
	}
	time.Sleep(30 * time.Millisecond)
	if got := claims.Load(); got != 4 {
		t.Fatalf("worker continued after NotAvailable: %d claims", got)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestServiceWorkerCompleteRetries(t *testing.T) {
	tests := []struct {
		name         string
		complete     func(int, context.Context) error
		wantCalls    int
		wantCloseErr bool
	}{
		{
			name: "transient",
			complete: func(call int, _ context.Context) error {
				if call < 3 {
					return errors.New("temporary completion failure")
				}
				return nil
			},
			wantCalls: 3,
		},
		{
			name: "permanent",
			complete: func(int, context.Context) error {
				return errors.New("permanent completion failure")
			},
			wantCalls:    completeMaxAttempts,
			wantCloseErr: true,
		},
		{
			name: "blocking",
			complete: func(_ int, ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			wantCalls:    completeMaxAttempts,
			wantCloseErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var claimed atomic.Bool
			var calls atomic.Int32
			attempted := make(chan struct{}, 1)
			backend := &workerTestBackend{
				claim: func(ctx context.Context) (Work, error) {
					if claimed.CompareAndSwap(false, true) {
						return asyncWork("work-1"), nil
					}
					<-ctx.Done()
					return Work{}, ctx.Err()
				},
				complete: func(ctx context.Context, _, _ string, _ sdkdelegation.Response) error {
					call := int(calls.Add(1))
					err := test.complete(call, ctx)
					if err == nil || call == completeMaxAttempts {
						select {
						case attempted <- struct{}{}:
						default:
						}
					}
					return err
				},
			}
			service, err := NewService(
				boundDirectory(t, completedEngine("done")),
				backend,
				WithMaxConcurrency(1),
			)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-attempted:
			case <-time.After(2 * time.Second):
				t.Fatal("completion attempts did not finish")
			}
			closeErr := service.Close()
			if (closeErr != nil) != test.wantCloseErr {
				t.Fatalf("Close error = %v, want error %v", closeErr, test.wantCloseErr)
			}
			if got := int(calls.Load()); got != test.wantCalls {
				t.Fatalf("Complete calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestServiceClosePersistsCanceledClaimBeforeReturning(t *testing.T) {
	started := make(chan struct{})
	completed := make(chan sdkdelegation.Response, 1)
	var claimed atomic.Bool
	backend := &workerTestBackend{
		claim: func(ctx context.Context) (Work, error) {
			if claimed.CompareAndSwap(false, true) {
				return asyncWork("shutdown-work"), nil
			}
			<-ctx.Done()
			return Work{}, ctx.Err()
		},
		complete: func(_ context.Context, _, _ string, response sdkdelegation.Response) error {
			completed <- response
			return nil
		},
	}
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		close(started)
		<-ctx.Done()
		return board, ctx.Err()
	})
	service, err := NewService(
		boundDirectory(t, engine),
		backend,
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case response := <-completed:
		if response.ID != "shutdown-work" ||
			response.Status != sdkdelegation.StatusCanceled {
			t.Fatalf("completed response = %+v", response)
		}
	default:
		t.Fatal("Close returned before persisting the canceled claim")
	}
}

func TestServiceWorkerUsesWorkContext(t *testing.T) {
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	started := make(chan struct{})
	completed := make(chan sdkdelegation.Response, 1)
	var claimed atomic.Bool
	backend := &workerTestBackend{
		claim: func(ctx context.Context) (Work, error) {
			if claimed.CompareAndSwap(false, true) {
				work := asyncWork("leased-work")
				work.Context = leaseCtx
				return work, nil
			}
			<-ctx.Done()
			return Work{}, ctx.Err()
		},
		complete: func(_ context.Context, _, _ string, response sdkdelegation.Response) error {
			completed <- response
			return nil
		},
	}
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		close(started)
		<-ctx.Done()
		return board, ctx.Err()
	})
	service, err := NewService(
		boundDirectory(t, engine),
		backend,
		WithMaxConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancelLease()
	select {
	case response := <-completed:
		if response.Status != sdkdelegation.StatusCanceled {
			t.Fatalf("completed response = %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("lease cancellation did not stop agent execution")
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
