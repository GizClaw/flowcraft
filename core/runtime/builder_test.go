package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/delegation/hostwrap"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

func TestBuildStartsRuntimeAndSessionsWork(t *testing.T) {
	bus := event.NewMemoryBus()
	reg := newBaseRegistry(t, bus, &recordingCheckpointStore{}, noopEngine())
	app, err := NewBuilder(reg).Build(context.Background(), baseRuntimeDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	result := runTurn(t, app.Sessions(), "bot", "conversation")
	if result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q, want completed", result.Status)
	}
}

func TestBuilderIsSingleUse(t *testing.T) {
	bus := event.NewMemoryBus()
	reg := newBaseRegistry(t, bus, &recordingCheckpointStore{}, noopEngine())
	builder := NewBuilder(reg)
	app, err := builder.Build(context.Background(), baseRuntimeDoc(t))
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if _, err := builder.Build(context.Background(), baseRuntimeDoc(t)); err == nil ||
		!strings.Contains(err.Error(), "already been used") {
		t.Fatalf("second Build error = %v", err)
	}
	if err := builder.WithHostFactory(func(f session.HostFactory) (session.HostFactory, error) {
		return f, nil
	}); !errors.Is(err, ErrBuilderUsed) {
		t.Fatalf("WithHostFactory after Build error = %v, want ErrBuilderUsed", err)
	}
	if err := builder.WithResultHostFactory(func(
		*deploy.Result,
		session.HostFactory,
	) (session.HostFactory, error) {
		return nil, nil
	}); !errors.Is(err, ErrBuilderUsed) {
		t.Fatalf("WithResultHostFactory after Build error = %v, want ErrBuilderUsed", err)
	}
}

func TestWithHostFactoryValidation(t *testing.T) {
	builder := NewBuilder(resource.NewRegistry())
	if err := builder.WithHostFactory(nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil decorator error = %v, want validation", err)
	}
	decorator := func(f session.HostFactory) (session.HostFactory, error) { return f, nil }
	if err := builder.WithHostFactory(decorator); err != nil {
		t.Fatalf("first decorator: %v", err)
	}
	if err := builder.WithHostFactory(decorator); !errdefs.IsValidation(err) {
		t.Fatalf("duplicate decorator error = %v, want validation", err)
	}
}

func TestWithResultHostFactoryValidation(t *testing.T) {
	builder := NewBuilder(resource.NewRegistry())
	if err := builder.WithResultHostFactory(nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil decorator error = %v, want validation", err)
	}
	decorator := func(
		*deploy.Result,
		session.HostFactory,
	) (session.HostFactory, error) {
		return nil, nil
	}
	if err := builder.WithResultHostFactory(decorator); err != nil {
		t.Fatalf("first decorator: %v", err)
	}
	if err := builder.WithResultHostFactory(decorator); !errdefs.IsValidation(err) {
		t.Fatalf("duplicate decorator error = %v, want validation", err)
	}
}

func TestBuildAppliesResultHostFactoryDecorator(t *testing.T) {
	var received *deploy.Result
	var applied bool
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	builder := NewBuilder(reg)
	if err := builder.WithResultHostFactory(func(
		result *deploy.Result,
		factory session.HostFactory,
	) (session.HostFactory, error) {
		received = result
		applied = true
		return factory, nil
	}); err != nil {
		t.Fatalf("WithResultHostFactory: %v", err)
	}

	app, err := builder.Build(context.Background(), baseRuntimeDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if !applied || received == nil {
		t.Fatalf("result host factory decorator not applied (applied=%v, result=%v)", applied, received)
	}
	if len(received.Names()) == 0 {
		t.Fatalf("decorator received empty deployment")
	}
	if result := runTurn(t, app.Sessions(), "bot", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
}

func TestBuildAbortsOnResultHostFactoryError(t *testing.T) {
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	builder := NewBuilder(reg)
	if err := builder.WithResultHostFactory(func(
		*deploy.Result,
		session.HostFactory,
	) (session.HostFactory, error) {
		return nil, errdefs.Validationf("boom")
	}); err != nil {
		t.Fatalf("WithResultHostFactory: %v", err)
	}
	_, err := builder.Build(context.Background(), baseRuntimeDoc(t))
	if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Build error = %v, want wrapped validation error containing boom", err)
	}
}

func TestWithLoaderValidation(t *testing.T) {
	builder := NewBuilder(resource.NewRegistry())
	if err := builder.WithLoader(nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil loader error = %v, want validation", err)
	}
	loader := resource.NewLoader(resource.WithBaseDir(t.TempDir()))
	if err := builder.WithLoader(loader); err != nil {
		t.Fatalf("first loader: %v", err)
	}
	if err := builder.WithLoader(loader); !errdefs.IsValidation(err) {
		t.Fatalf("duplicate loader error = %v, want validation", err)
	}
}

func TestBuildRejectsInvalidEventBus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bus    any
		mutate func(*deploy.Document)
	}{
		{
			name: "missing resource",
			bus:  event.NewMemoryBus(),
			mutate: func(doc *deploy.Document) {
				delete(doc.Resources, "events")
			},
		},
		{name: "wrong Go type", bus: 42},
		{name: "typed nil", bus: (*event.MemoryBus)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newBaseRegistry(t, tc.bus, &recordingCheckpointStore{}, noopEngine())
			doc := baseRuntimeDoc(t)
			if tc.mutate != nil {
				tc.mutate(&doc)
			}
			if _, err := NewBuilder(reg).Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuildRejectsInvalidCheckpointStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  any
		mutate func(*deploy.Document)
	}{
		{
			name:  "missing resource",
			store: &recordingCheckpointStore{},
			mutate: func(doc *deploy.Document) {
				delete(doc.Resources, "cps")
			},
		},
		{name: "wrong Go type", store: 42},
		{name: "typed nil", store: (*recordingCheckpointStore)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newBaseRegistry(t, event.NewMemoryBus(), tc.store, noopEngine())
			doc := baseRuntimeDoc(t)
			if tc.mutate != nil {
				tc.mutate(&doc)
			}
			if _, err := NewBuilder(reg).Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuildWiresCheckpointStoreResource(t *testing.T) {
	store := &recordingCheckpointStore{}
	engine := withRunEnd(agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := host.Checkpoint(ctx, agent.Checkpoint{
			ExecID: run.RunID,
			Steps:  []string{"wave"},
			Board:  board.Snapshot(),
		}); err != nil {
			return board, err
		}
		return board, nil
	}))
	reg := newBaseRegistry(t, event.NewMemoryBus(), store, engine)
	app, err := NewBuilder(reg).Build(context.Background(), baseRuntimeDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	result := runTurn(t, app.Sessions(), "bot", "conv")
	if result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
	store.mu.Lock()
	saved := store.saved
	store.mu.Unlock()
	if saved == nil || len(saved.Steps) != 1 {
		t.Fatalf("store.saved = %+v, want a saved checkpoint", saved)
	}
}

func TestWithHostFactoryWrapsBaseHostAndReportsUsage(t *testing.T) {
	engine := withRunEnd(agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := host.ReportUsage(ctx, inference.Usage{
			InputTokens:  10,
			OutputTokens: 7,
			TotalTokens:  17,
		}); err != nil {
			return board, err
		}
		return board, nil
	}))
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, engine)
	builder := NewBuilder(reg)

	var mu sync.Mutex
	var usage []inference.Usage
	if err := builder.WithHostFactory(func(base session.HostFactory) (session.HostFactory, error) {
		if base == nil {
			t.Fatal("decorator received nil base host factory")
		}
		return session.HostFactoryFunc(func(ctx context.Context, request session.HostRequest) (agent.Host, error) {
			host, err := base.NewHost(ctx, request)
			if err != nil {
				return nil, err
			}
			return agent.HostFuncs{
				Inner: host,
				ReportUsageFn: func(_ context.Context, u inference.Usage) error {
					mu.Lock()
					usage = append(usage, u)
					mu.Unlock()
					return host.ReportUsage(ctx, u)
				},
			}, nil
		}), nil
	}); err != nil {
		t.Fatalf("WithHostFactory: %v", err)
	}

	app, err := builder.Build(context.Background(), baseRuntimeDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if result := runTurn(t, app.Sessions(), "bot", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(usage) != 1 || usage[0].InputTokens != 10 || usage[0].OutputTokens != 7 {
		t.Fatalf("usage = %+v, want one report", usage)
	}
}

func TestConcurrentCloseReturnsSameAggregateAndResultLast(t *testing.T) {
	var mu sync.Mutex
	var log []string
	busErr := errors.New("bus close failed")
	bus := &trackedBus{
		MemoryBus: event.NewMemoryBus(), mu: &mu, log: &log, closeErr: busErr,
	}
	reg := newBaseRegistry(t, bus, &recordingCheckpointStore{}, noopEngine())
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: "test.CloseLog", Impl: "tracked"},
		value: &closingValue{log: &log, mu: &mu},
	})
	doc := baseRuntimeDoc(t)
	doc.Resources["extra"] = resource.Resource{Kind: "test.CloseLog", Impl: "tracked"}
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	const callers = 16
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			errs <- app.Close()
		}()
	}
	wait.Wait()
	close(errs)
	var first error
	for closeErr := range errs {
		if !errors.Is(closeErr, busErr) {
			t.Fatalf("Close error lost aggregate member: %v", closeErr)
		}
		if first == nil {
			first = closeErr
		} else if fmt.Sprintf("%p", first) != fmt.Sprintf("%p", closeErr) {
			t.Fatalf("concurrent Close did not return cached error: %p != %p", first, closeErr)
		}
	}
	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	if extraClose := indexOf(got, "extra.close"); extraClose < 0 ||
		extraClose > indexOf(got, "bus.close") {
		t.Fatalf("deployment resources were not closed in reverse order: %v", got)
	}
	if _, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "after-close",
	}); !errors.Is(err, session.ErrManagerClosed) {
		t.Fatalf("manager accepted work after close: %v", err)
	}
}

type closingValue struct {
	mu  *sync.Mutex
	log *[]string
}

func (v *closingValue) Close() error {
	v.mu.Lock()
	*v.log = append(*v.log, "extra.close")
	v.mu.Unlock()
	return nil
}

type fakeDelegationService struct {
	id string
}

func (s *fakeDelegationService) Delegate(
	context.Context,
	delegation.Request,
) (delegation.Response, error) {
	return delegation.Response{}, nil
}

func (s *fakeDelegationService) Get(context.Context, string) (delegation.Response, error) {
	return delegation.Response{}, nil
}

func TestBuildWrapsDelegationServiceWhenRequested(t *testing.T) {
	service := &fakeDelegationService{id: "local"}
	var got delegation.Service
	engine := withRunEnd(agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		svc, ok := delegation.ServiceFromHost(host)
		if !ok {
			return board, errors.New("delegation service missing from host")
		}
		got = svc
		return board, nil
	}))
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, engine)
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: delegation.ServiceKind, Impl: "local"},
		value: service,
	})
	doc := baseRuntimeDoc(t)
	doc.Resources["delegation"] = resource.Resource{
		Kind: delegation.ServiceKind, Impl: "local",
	}

	builder := NewBuilder(reg)
	if err := builder.WithResultHostFactory(func(
		result *deploy.Result,
		factory session.HostFactory,
	) (session.HostFactory, error) {
		return hostwrap.Wrap(factory, result)
	}); err != nil {
		t.Fatalf("WithResultHostFactory: %v", err)
	}
	app, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if result := runTurn(t, app.Sessions(), "bot", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
	if got != service {
		t.Fatalf("engine received service %p, want %p", got, service)
	}
}

func TestBuildWithoutResultHostFactoryLeavesHostUnexposed(t *testing.T) {
	service := &fakeDelegationService{id: "local"}
	engine := withRunEnd(agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if _, ok := delegation.ServiceFromHost(host); ok {
			return board, errors.New("delegation service unexpectedly exposed on host")
		}
		return board, nil
	}))
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, engine)
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: delegation.ServiceKind, Impl: "local"},
		value: service,
	})
	doc := baseRuntimeDoc(t)
	doc.Resources["delegation"] = resource.Resource{
		Kind: delegation.ServiceKind, Impl: "local",
	}

	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if result := runTurn(t, app.Sessions(), "bot", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q", result.Status)
	}
}

func TestBuildBindsDelegationDirectory(t *testing.T) {
	directory := delegation.NewDirectory()
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	reg.MustRegister(delegation.NewServiceFactory(directory))
	doc := baseRuntimeDoc(t)
	doc.Resources["delegation"] = resource.Resource{
		Kind: delegation.ServiceKind, Impl: "local",
	}

	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	targets, err := directory.List(context.Background())
	if err != nil {
		t.Fatalf("directory was not bound: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != "bot" {
		t.Fatalf("bound targets = %+v, want [bot]", targets)
	}
}

func TestBuildRejectsNilRegistryAndNilContext(t *testing.T) {
	if _, err := NewBuilder(nil).Build(context.Background(), baseRuntimeDoc(t)); !errdefs.IsValidation(err) {
		t.Fatalf("nil registry error = %v, want validation", err)
	}
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	//nolint:staticcheck // deliberate: nil context must be rejected
	if _, err := NewBuilder(reg).Build(nil, baseRuntimeDoc(t)); !errdefs.IsValidation(err) {
		t.Fatalf("nil context error = %v, want validation", err)
	}
}

func TestBuildRollsBackResultOnFailure(t *testing.T) {
	var mu sync.Mutex
	var log []string
	bus := &trackedBus{
		MemoryBus: event.NewMemoryBus(), mu: &mu, log: &log,
	}
	// The event bus factory returns the tracked bus, then the runtime
	// fails resolving the checkpoint store (wrong Go type) and must
	// close the deployment result, which closes the bus once.
	reg := newBaseRegistry(t, bus, 42, noopEngine())
	if _, err := NewBuilder(reg).Build(context.Background(), baseRuntimeDoc(t)); err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}
	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "bus.close" {
		t.Fatalf("rollback close log = %v, want single bus.close", got)
	}
}

func TestRuntimeSessionsAreNilSafe(t *testing.T) {
	var app *Runtime
	if app.Sessions() != nil {
		t.Fatal("nil Runtime returned a non-nil manager")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("nil Runtime Close = %v", err)
	}
}
