package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/GizClaw/flowcraft/sdkx/scheduler"
)

type testEngineFactory struct {
	engine agent.Engine
}

func (f testEngineFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: "test"}
}

func (f testEngineFactory) New(context.Context, sdkconfig.Input) (any, error) {
	return f.engine, nil
}

type testResourceFactory struct {
	spec  sdkconfig.Spec
	value any
	err   error
	calls *int
}

func (f testResourceFactory) Spec() sdkconfig.Spec { return f.spec }
func (f testResourceFactory) New(context.Context, sdkconfig.Input) (any, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.value, f.err
}

type trackedBus struct {
	*event.MemoryBus
	log      *[]string
	closeErr error
	mu       *sync.Mutex
}

func (b *trackedBus) Close() error {
	b.mu.Lock()
	*b.log = append(*b.log, "result.close")
	b.mu.Unlock()
	_ = b.MemoryBus.Close()
	return b.closeErr
}

type testIntegrationFactory struct {
	spec        IntegrationSpec
	log         *[]string
	mu          *sync.Mutex
	failPhase   string
	failName    string
	closeErr    error
	rollbackErr error
	rollback    func(context.Context, string) error
	typedNil    string
}

func (f *testIntegrationFactory) Spec() IntegrationSpec { return f.spec }

func (f *testIntegrationFactory) Prepare(_ context.Context, input PrepareInput) (PreparedIntegration, error) {
	f.add("prepare." + input.Name)
	if f.typedNil == "prepared" && input.Name == f.failName {
		var prepared *testLifecycleIntegration
		return prepared, nil
	}
	prepared := &testLifecycleIntegration{factory: f, name: input.Name}
	if f.failPhase == "prepare" && input.Name == f.failName {
		return prepared, errors.New("prepare failed")
	}
	return prepared, nil
}

func (f *testIntegrationFactory) add(value string) {
	f.mu.Lock()
	*f.log = append(*f.log, value)
	f.mu.Unlock()
}

type testLifecycleIntegration struct {
	factory *testIntegrationFactory
	name    string
}

func (i *testLifecycleIntegration) Bind(_ context.Context, input BindInput) error {
	i.factory.add("bind." + i.name)
	if i.factory.failPhase == "bind" && i.name == i.factory.failName {
		return errors.New("bind failed")
	}
	if _, ok := input.Dependencies.Get("store"); len(i.factory.spec.Deps) > 0 && !ok {
		return errors.New("store was not bound")
	}
	return nil
}

func (i *testLifecycleIntegration) DecorateHost(inner session.HostFactory) (session.HostFactory, error) {
	i.factory.add("decorate." + i.name)
	if i.factory.failPhase == "decorate" && i.name == i.factory.failName {
		return nil, errors.New("decorate failed")
	}
	if i.factory.typedNil == "host" && i.name == i.factory.failName {
		var factory *loggingHostFactory
		return factory, nil
	}
	return &loggingHostFactory{inner: inner, name: i.name, add: i.factory.add}, nil
}

func (i *testLifecycleIntegration) Start(context.Context) error {
	i.factory.add("start." + i.name)
	if i.factory.failPhase == "start" && i.name == i.factory.failName {
		return errors.New("start failed")
	}
	return nil
}

func (i *testLifecycleIntegration) Close() error {
	i.factory.add("close." + i.name)
	return i.factory.closeErr
}

func (i *testLifecycleIntegration) Rollback(ctx context.Context) error {
	i.factory.add("rollback." + i.name)
	if i.factory.rollback != nil {
		return i.factory.rollback(ctx, i.name)
	}
	return i.factory.rollbackErr
}

type loggingHostFactory struct {
	inner session.HostFactory
	name  string
	add   func(string)
}

func (f *loggingHostFactory) NewHost(ctx context.Context, request session.HostRequest) (agent.Host, error) {
	f.add("host." + f.name)
	return f.inner.NewHost(ctx, request)
}

type schedulerProbeFactory struct {
	server *trackedSchedulerServer
	log    *[]string
	mu     *sync.Mutex
}

func (f *schedulerProbeFactory) Spec() IntegrationSpec {
	return IntegrationSpec{
		Kind: "scheduler-probe",
		Deps: []DependencySpec{{
			Name:     "scheduler",
			Kind:     schedulerconfig.ResourceKind,
			Type:     reflect.TypeFor[sdkscheduler.Server](),
			Required: true,
		}},
	}
}

func (f *schedulerProbeFactory) Prepare(context.Context, PrepareInput) (PreparedIntegration, error) {
	return &schedulerProbeIntegration{factory: f}, nil
}

type schedulerProbeIntegration struct {
	factory *schedulerProbeFactory
	server  sdkscheduler.Server
}

func (i *schedulerProbeIntegration) Bind(_ context.Context, input BindInput) error {
	server, err := DependencyAs[sdkscheduler.Server](input.Dependencies, "scheduler")
	if err != nil {
		return err
	}
	i.server = server
	return nil
}

func (i *schedulerProbeIntegration) DecorateHost(factory session.HostFactory) (session.HostFactory, error) {
	return factory, nil
}

func (i *schedulerProbeIntegration) Start(context.Context) error {
	if i.factory.server != nil && i.factory.server.started {
		return errors.New("scheduler started before integrations completed Start")
	}
	i.factory.add("integration.start")
	return nil
}

func (i *schedulerProbeIntegration) Close() error {
	if i.factory.server != nil && i.factory.server.closed {
		return errors.New("scheduler closed before integration")
	}
	i.factory.add("integration.close")
	return nil
}

func (f *schedulerProbeFactory) add(value string) {
	f.mu.Lock()
	*f.log = append(*f.log, value)
	f.mu.Unlock()
}

type trackedSchedulerServer struct {
	sdkscheduler.Server
	log        *[]string
	mu         *sync.Mutex
	started    bool
	closed     bool
	closeCalls int
}

func (s *trackedSchedulerServer) Start() error {
	if starter, ok := s.Server.(interface{ Start() error }); ok {
		if err := starter.Start(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.started = true
	*s.log = append(*s.log, "scheduler.start")
	s.mu.Unlock()
	return nil
}

func (s *trackedSchedulerServer) Close() error {
	s.mu.Lock()
	s.closed = true
	s.closeCalls++
	*s.log = append(*s.log, "scheduler.close")
	s.mu.Unlock()
	if closer, ok := s.Server.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func newDeployBuilder(t *testing.T, bus any, store any, busErr error) *deploy.Builder {
	t.Helper()
	builder := deploy.NewBuilder()
	builder.MustRegisterEngine(testEngineFactory{engine: agent.EngineFunc(
		func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
			return board, nil
		},
	)})
	if err := builder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: eventBusResourceKind, Impl: "test"},
		value: bus,
		err:   busErr,
	}); err != nil {
		t.Fatal(err)
	}
	if err := builder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: "test.Store", Impl: "test"},
		value: store,
	}); err != nil {
		t.Fatal(err)
	}
	return builder
}

func registerSchedulerResource(t *testing.T, builder *deploy.Builder, value any) {
	t.Helper()
	if err := builder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: schedulerconfig.ResourceKind, Impl: "test"},
		value: value,
	}); err != nil {
		t.Fatal(err)
	}
}

func runtimeDoc(t *testing.T, integrations string) deploy.Document {
	t.Helper()
	text := `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  data: {kind: test.Store, impl: test, export: true}
agents:
  bot:
    engine: {kind: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1s, sink_buffer: 8}
` + integrations
	doc, err := deploy.Parse([]byte(text))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func schedulerRuntimeDoc(t *testing.T, schedulerName, integrations string) deploy.Document {
	t.Helper()
	schedulerConfig := ""
	if schedulerName != "" {
		schedulerConfig = "  scheduler: " + schedulerName + "\n"
	}
	text := `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  data: {kind: test.Store, impl: test, export: true}
  primary: {kind: scheduler.Server, impl: test}
  secondary: {kind: scheduler.Server, impl: test}
agents:
  bot:
    engine: {kind: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1s, sink_buffer: 8}
` + schedulerConfig + integrations
	doc, err := deploy.Parse([]byte(text))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func configuredIntegrations() string {
	return `  integrations:
    - {name: a, kind: lifecycle, deps: {store: data}}
    - {name: b, kind: lifecycle, deps: {store: data}}
`
}

func registerLifecycle(t *testing.T, builder *Builder, factory *testIntegrationFactory) {
	t.Helper()
	if err := builder.RegisterIntegration(factory); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
}

func TestBuildRollsBackEveryLifecyclePhase(t *testing.T) {
	for _, phase := range []string{"prepare", "build", "bind", "decorate", "start"} {
		t.Run(phase, func(t *testing.T) {
			var mu sync.Mutex
			var log []string
			bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
			var buildErr error
			if phase == "build" {
				buildErr = errors.New("build failed")
			}
			deployBuilder := newDeployBuilder(t, bus, &struct{}{}, buildErr)
			builder := NewBuilder(deployBuilder)
			factory := &testIntegrationFactory{
				spec: IntegrationSpec{Kind: "lifecycle", Deps: []DependencySpec{{
					Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true,
				}}},
				log: &log, mu: &mu, failPhase: phase, failName: "b",
			}
			registerLifecycle(t, builder, factory)
			if _, err := builder.Build(context.Background(), runtimeDoc(t, configuredIntegrations())); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}

			mu.Lock()
			got := append([]string(nil), log...)
			mu.Unlock()
			closeA, closeB := indexOf(got, "close.a"), indexOf(got, "close.b")
			if closeA < 0 || closeB < 0 || closeB > closeA {
				t.Fatalf("integrations not rolled back in reverse order: %v", got)
			}
			rollbackA, rollbackB := indexOf(got, "rollback.a"), indexOf(got, "rollback.b")
			if rollbackA < 0 || rollbackB < 0 || rollbackB > rollbackA {
				t.Fatalf("build compensation not run in reverse order: %v", got)
			}
			if rollbackA > closeB {
				t.Fatalf("ordinary close began before compensation completed: %v", got)
			}
			if resultClose := indexOf(got, "result.close"); resultClose >= 0 && resultClose < closeA {
				t.Fatalf("deployment result closed before integrations: %v", got)
			}
		})
	}
}

func TestBuildJoinsPrimaryRollbackAndCloseFailures(t *testing.T) {
	var mu sync.Mutex
	var log []string
	rollbackErr := errors.New("rollback failed")
	closeErr := errors.New("close failed")
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	builder := NewBuilder(newDeployBuilder(t, bus, &struct{}{}, nil))
	registerLifecycle(t, builder, &testIntegrationFactory{
		spec:        IntegrationSpec{Kind: "lifecycle"},
		log:         &log,
		mu:          &mu,
		failPhase:   "bind",
		failName:    "a",
		rollbackErr: rollbackErr,
		closeErr:    closeErr,
	})

	_, err := builder.Build(context.Background(), runtimeDoc(t,
		"  integrations:\n    - {name: a, kind: lifecycle}\n"))
	if err == nil || !strings.Contains(err.Error(), "bind failed") ||
		!errors.Is(err, rollbackErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Build error = %v, want primary, rollback, and close failures", err)
	}
	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	if indexOf(got, "rollback.a") < 0 ||
		indexOf(got, "rollback.a") > indexOf(got, "close.a") {
		t.Fatalf("build rollback did not precede ordinary close: %v", got)
	}
}

func TestBuildRollbackUsesIndependentLiveContextsAfterBuildCancellation(t *testing.T) {
	var mu sync.Mutex
	var log []string
	var deadlines = make(map[string]time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rollbackAErr := errors.New("a rollback failed")
	rollbackBErr := errors.New("b rollback failed")
	builder := NewBuilder(newDeployBuilder(
		t,
		&trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu},
		&struct{}{},
		nil,
	))
	factory := &testIntegrationFactory{
		spec: IntegrationSpec{Kind: "lifecycle", Deps: []DependencySpec{{
			Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true,
		}}},
		log: &log, mu: &mu, failPhase: "bind", failName: "b",
		rollback: func(rollbackCtx context.Context, name string) error {
			if err := rollbackCtx.Err(); err != nil {
				return fmt.Errorf("%s rollback context unavailable: %w", name, err)
			}
			deadline, ok := rollbackCtx.Deadline()
			if !ok {
				return fmt.Errorf("%s rollback context has no deadline", name)
			}
			mu.Lock()
			deadlines[name] = deadline
			mu.Unlock()
			if name == "b" {
				time.Sleep(100 * time.Millisecond)
				return rollbackBErr
			}
			return rollbackAErr
		},
	}
	registerLifecycle(t, builder, factory)

	_, err := builder.Build(ctx, runtimeDoc(t, configuredIntegrations()))
	if err == nil || !strings.Contains(err.Error(), "bind failed") ||
		!errors.Is(err, rollbackAErr) || !errors.Is(err, rollbackBErr) {
		t.Fatalf("Build error = %v, want bind and both rollback failures", err)
	}
	mu.Lock()
	deadlineA, okA := deadlines["a"]
	deadlineB, okB := deadlines["b"]
	mu.Unlock()
	if !okA || !okB {
		t.Fatalf("rollback deadlines = %v, want both integrations attempted", deadlines)
	}
	if deadlineA.Sub(deadlineB) < 80*time.Millisecond {
		t.Fatalf("rollback deadlines differ by %v, want independent deadline after slow predecessor",
			deadlineA.Sub(deadlineB))
	}
}

func TestBuildRejectsDependencyAndNilMismatches(t *testing.T) {
	var mu sync.Mutex
	var log []string
	cases := []struct {
		name      string
		docMutate func(*deploy.Document)
		store     any
		spec      DependencySpec
		config    string
		typedNil  string
	}{
		{
			name: "undeclared", store: &struct{}{},
			spec:   DependencySpec{Name: "other", Kind: "test.Store", Type: reflect.TypeFor[*struct{}]()},
			config: configuredIntegrations(),
		},
		{
			name: "missing required", store: &struct{}{},
			spec:   DependencySpec{Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle}\n",
		},
		{
			name: "kind mismatch", store: &struct{}{},
			spec:   DependencySpec{Name: "store", Kind: "wrong.Kind", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: data}}\n",
		},
		{
			name: "Go type mismatch", store: 42,
			spec:   DependencySpec{Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: data}}\n",
		},
		{
			name: "typed nil dependency", store: (*struct{})(nil),
			spec:   DependencySpec{Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: data}}\n",
		},
		{
			name: "typed nil prepared", store: &struct{}{}, typedNil: "prepared",
			spec:   DependencySpec{Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: data}}\n",
		},
		{
			name: "typed nil host", store: &struct{}{}, typedNil: "host",
			spec:   DependencySpec{Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true},
			config: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: data}}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log = nil
			bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
			builder := NewBuilder(newDeployBuilder(t, bus, tc.store, nil))
			factory := &testIntegrationFactory{
				spec: IntegrationSpec{Kind: "lifecycle", Deps: []DependencySpec{tc.spec}},
				log:  &log, mu: &mu, typedNil: tc.typedNil, failName: "a",
			}
			registerLifecycle(t, builder, factory)
			doc := runtimeDoc(t, tc.config)
			if tc.docMutate != nil {
				tc.docMutate(&doc)
			}
			if _, err := builder.Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuildRejectsMissingUnknownAndInvalidEventResources(t *testing.T) {
	var mu sync.Mutex
	for _, tc := range []struct {
		name       string
		bus        any
		integrates string
		mutate     func(*deploy.Document)
		register   bool
	}{
		{
			name: "missing event resource", bus: event.NewMemoryBus(),
			mutate: func(doc *deploy.Document) { delete(doc.Resources, "events") },
		},
		{
			name: "wrong event kind", bus: event.NewMemoryBus(),
			mutate: func(doc *deploy.Document) {
				entry := doc.Resources["events"]
				entry.Kind = "wrong.Bus"
				doc.Resources["events"] = entry
			},
		},
		{name: "wrong event Go type", bus: 42},
		{
			name: "unknown integration kind", bus: event.NewMemoryBus(),
			integrates: "  integrations:\n    - {name: a, kind: unknown}\n",
		},
		{
			name: "missing dependency resource", bus: event.NewMemoryBus(), register: true,
			integrates: "  integrations:\n    - {name: a, kind: lifecycle, deps: {store: absent}}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			builder := NewBuilder(newDeployBuilder(t, tc.bus, &struct{}{}, nil))
			if tc.register {
				registerLifecycle(t, builder, &testIntegrationFactory{
					spec: IntegrationSpec{Kind: "lifecycle", Deps: []DependencySpec{{
						Name: "store", Kind: "test.Store",
						Type: reflect.TypeFor[*struct{}](), Required: true,
					}}},
					log: &log, mu: &mu,
				})
			}
			doc := runtimeDoc(t, tc.integrates)
			if tc.mutate != nil {
				tc.mutate(&doc)
			}
			if _, err := builder.Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuildValidatesConfiguredSchedulerResource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		mutate func(*deploy.Document)
	}{
		{
			name: "missing resource",
			mutate: func(doc *deploy.Document) {
				delete(doc.Resources, "primary")
			},
		},
		{
			name: "wrong resource kind",
			mutate: func(doc *deploy.Document) {
				entry := doc.Resources["primary"]
				entry.Kind = "wrong.Service"
				doc.Resources["primary"] = entry
			},
		},
		{name: "wrong Go type", value: 42},
		{name: "typed nil", value: (*scheduler.LocalServer)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var log []string
			bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
			deployment := newDeployBuilder(t, bus, &struct{}{}, nil)
			if tc.mutate == nil {
				registerSchedulerResource(t, deployment, tc.value)
			}
			doc := schedulerRuntimeDoc(t, "primary", "")
			delete(doc.Resources, "secondary")
			if tc.mutate != nil {
				tc.mutate(&doc)
			}
			if _, err := NewBuilder(deployment).Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuildRejectsDeployResourceDependingOnRuntimeSchedulerBeforeBuild(t *testing.T) {
	var mu sync.Mutex
	var log []string
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	deployment := newDeployBuilder(
		t,
		&trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu},
		&struct{}{},
		nil,
	)
	schedulerCalls := 0
	if err := deployment.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{
			Kind: schedulerconfig.ResourceKind,
			Impl: "test",
		},
		value: local,
		calls: &schedulerCalls,
	}); err != nil {
		t.Fatal(err)
	}
	consumerCalls := 0
	if err := deployment.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{
			Kind: "test.Consumer",
			Impl: "test",
			Deps: []sdkconfig.DepSpec{{
				Name: "scheduler", Type: schedulerconfig.ResourceKind, Required: true,
			}},
		},
		value: &struct{}{},
		calls: &consumerCalls,
	}); err != nil {
		t.Fatal(err)
	}
	doc := schedulerRuntimeDoc(t, "primary", "")
	delete(doc.Resources, "secondary")
	doc.Resources["consumer"] = deploy.ResourceEntry{
		Kind: "test.Consumer",
		Impl: "test",
		Deps: map[string]deploy.DepRef{
			"scheduler": {Resource: "primary"},
		},
	}

	if _, err := NewBuilder(deployment).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if schedulerCalls != 0 || consumerCalls != 0 {
		t.Fatalf(
			"resource constructors ran before runtime validation: scheduler=%d consumer=%d",
			schedulerCalls, consumerCalls,
		)
	}
}

func TestRuntimeSchedulerMayDependOnOtherDeployResource(t *testing.T) {
	var mu sync.Mutex
	var log []string
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	deployment := newDeployBuilder(
		t,
		&trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu},
		&struct{}{},
		nil,
	)
	if err := deployment.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{
			Kind: schedulerconfig.ResourceKind,
			Impl: "test",
			Deps: []sdkconfig.DepSpec{{
				Name: "store", Type: "test.Store", Required: true,
			}},
		},
		value: local,
	}); err != nil {
		t.Fatal(err)
	}
	doc := schedulerRuntimeDoc(t, "primary", "")
	delete(doc.Resources, "secondary")
	primary := doc.Resources["primary"]
	primary.Deps = map[string]deploy.DepRef{
		"store": {Resource: "data"},
	}
	doc.Resources["primary"] = primary

	app, err := NewBuilder(deployment).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSchedulerDependenciesUseOnlyRuntimeScheduler(t *testing.T) {
	for _, tc := range []struct {
		name          string
		schedulerName string
		required      bool
		deps          string
	}{
		{
			name:     "declared without runtime scheduler",
			required: true,
		},
		{
			name:     "bound without runtime scheduler",
			required: true,
			deps:     ", deps: {scheduler: primary}",
		},
		{
			name:          "bound to a second scheduler",
			schedulerName: "primary",
			required:      true,
			deps:          ", deps: {scheduler: secondary}",
		},
		{
			name:          "explicitly bound to runtime scheduler",
			schedulerName: "primary",
			required:      true,
			deps:          ", deps: {scheduler: primary}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var log []string
			builder := NewBuilder(newDeployBuilder(
				t, &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu},
				&struct{}{}, nil,
			))
			err := builder.RegisterIntegration(&testIntegrationFactory{
				spec: IntegrationSpec{
					Kind: "scheduler-consumer",
					Deps: []DependencySpec{{
						Name:     "scheduler",
						Kind:     schedulerconfig.ResourceKind,
						Type:     reflect.TypeFor[sdkscheduler.Server](),
						Required: tc.required,
					}},
				},
				log: &log, mu: &mu,
			})
			if err != nil {
				t.Fatal(err)
			}
			integrations := "  integrations:\n    - {name: consumer, kind: scheduler-consumer" + tc.deps + "}\n"
			if _, err := builder.Build(
				context.Background(),
				schedulerRuntimeDoc(t, tc.schedulerName, integrations),
			); !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want validation", err)
			}
		})
	}
}

func TestSchedulerStartsAfterIntegrationsAndClosesAfterThem(t *testing.T) {
	var mu sync.Mutex
	var log []string
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	server := &trackedSchedulerServer{Server: local, log: &log, mu: &mu}
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	deployment := newDeployBuilder(t, bus, &struct{}{}, nil)
	registerSchedulerResource(t, deployment, server)
	builder := NewBuilder(deployment)
	if err := builder.RegisterIntegration(&schedulerProbeFactory{
		server: server, log: &log, mu: &mu,
	}); err != nil {
		t.Fatal(err)
	}
	doc := schedulerRuntimeDoc(t, "primary",
		"  integrations:\n    - {name: probe, kind: scheduler-probe}\n")
	delete(doc.Resources, "secondary")
	app, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertControlFacade(t, app.Scheduler())
	if !server.started {
		t.Fatal("scheduler was not started after integrations completed Start")
	}
	if got := append([]string(nil), log...); indexOf(got, "integration.start") > indexOf(got, "scheduler.start") {
		t.Fatalf("scheduler started before integration worker: %v", got)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatalf("repeated scheduler Close: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	if indexOf(got, "integration.close") < 0 {
		t.Fatalf("integration did not close: %v", got)
	}
	if indexOf(got, "integration.close") > indexOf(got, "scheduler.close") {
		t.Fatalf("scheduler closed before integration: %v", got)
	}
	if indexOf(got, "scheduler.close") > indexOf(got, "result.close") {
		t.Fatalf("deployment closed before scheduler: %v", got)
	}
	if server.closeCalls != 1 {
		t.Fatalf("scheduler close calls = %d, want 1", server.closeCalls)
	}
}

type remoteSchedulerProxy struct {
	sdkscheduler.Server
}

func TestRemoteSchedulerWithoutLifecycleBuildsAndCloses(t *testing.T) {
	var mu sync.Mutex
	var log []string
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	proxy := &remoteSchedulerProxy{Server: local}
	deployment := newDeployBuilder(
		t,
		&trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu},
		&struct{}{},
		nil,
	)
	registerSchedulerResource(t, deployment, proxy)
	doc := schedulerRuntimeDoc(t, "primary", "")
	delete(doc.Resources, "secondary")
	app, err := NewBuilder(deployment).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertControlFacade(t, app.Scheduler())
	if _, err := app.Scheduler().ListRules(context.Background(), "forwarded"); err != nil {
		t.Fatalf("Scheduler().ListRules: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := local.ListRules(context.Background(), "still-open"); err != nil {
		t.Fatalf("runtime closed remote scheduler: %v", err)
	}
}

func TestBuildRollbackClosesIntegrationBeforeSchedulerOnce(t *testing.T) {
	var mu sync.Mutex
	var log []string
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	server := &trackedSchedulerServer{Server: local, log: &log, mu: &mu}
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	deployment := newDeployBuilder(t, bus, &struct{}{}, nil)
	registerSchedulerResource(t, deployment, server)
	builder := NewBuilder(deployment)
	registerLifecycle(t, builder, &testIntegrationFactory{
		spec: IntegrationSpec{Kind: "lifecycle"},
		log:  &log, mu: &mu, failPhase: "start", failName: "a",
	})
	doc := schedulerRuntimeDoc(t, "primary",
		"  integrations:\n    - {name: a, kind: lifecycle}\n")
	delete(doc.Resources, "secondary")

	if _, err := builder.Build(context.Background(), doc); err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}

	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	if indexOf(got, "close.a") < 0 || indexOf(got, "close.a") > indexOf(got, "scheduler.close") {
		t.Fatalf("rollback closed scheduler before integration: %v", got)
	}
	if server.closeCalls != 1 {
		t.Fatalf("rollback scheduler close calls = %d, want 1; log = %v", server.closeCalls, got)
	}
}

func assertControlFacade(t *testing.T, control sdkscheduler.Control) {
	t.Helper()
	if control == nil {
		t.Fatal("Scheduler() returned nil")
	}
	if _, ok := control.(sdkscheduler.Server); ok {
		t.Fatalf("Scheduler() dynamic type %T exposes scheduler.Server", control)
	}
	if _, ok := control.(sdkscheduler.WorkSource); ok {
		t.Fatalf("Scheduler() dynamic type %T exposes scheduler.WorkSource", control)
	}
	if _, ok := control.(interface{ Close() error }); ok {
		t.Fatalf("Scheduler() dynamic type %T exposes Close", control)
	}
	if _, ok := control.(interface{ Start() error }); ok {
		t.Fatalf("Scheduler() dynamic type %T exposes Start", control)
	}
}

func TestDecoratorOrderBaseHostAndRealSession(t *testing.T) {
	var mu sync.Mutex
	var log []string
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	builder := NewBuilder(newDeployBuilder(t, bus, &struct{}{}, nil))
	factory := &testIntegrationFactory{
		spec: IntegrationSpec{Kind: "lifecycle", Deps: []DependencySpec{{
			Name: "store", Kind: "test.Store", Type: reflect.TypeFor[*struct{}](), Required: true,
		}}},
		log: &log, mu: &mu,
	}
	registerLifecycle(t, builder, factory)
	runtime, err := builder.Build(context.Background(), runtimeDoc(t, configuredIntegrations()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	lease, err := runtime.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "conversation",
	})
	if err != nil {
		t.Fatalf("Sessions.Open: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Session.Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Turn.Wait: %v", err)
	}
	_ = lease.Close()

	mu.Lock()
	got := append([]string(nil), log...)
	mu.Unlock()
	hostA, hostB := indexOf(got, "host.a"), indexOf(got, "host.b")
	if hostA < 0 || hostB < 0 || hostA > hostB {
		t.Fatalf("first configured decorator is not outermost: %v", got)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}
}

func TestWithHostFactoryWrapsBaseHostAndReportsUsage(t *testing.T) {
	var mu sync.Mutex
	var log []string
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	deployBuilder := deploy.NewBuilder()
	deployBuilder.MustRegisterEngine(testEngineFactory{engine: agent.EngineFunc(
		func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			if err := host.ReportUsage(ctx, inference.Usage{
				InputTokens:  10,
				OutputTokens: 7,
				TotalTokens:  17,
			}); err != nil {
				return nil, err
			}
			return board, nil
		},
	)})
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: eventBusResourceKind, Impl: "test"},
		value: bus,
	}); err != nil {
		t.Fatal(err)
	}
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: "test.Store", Impl: "test"},
		value: &struct{}{},
	}); err != nil {
		t.Fatal(err)
	}

	builder := NewBuilder(deployBuilder)
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

	runtime, err := builder.Build(context.Background(), runtimeDoc(t, ""))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	lease, err := runtime.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "conversation",
	})
	if err != nil {
		t.Fatalf("Sessions.Open: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Session.Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Turn.Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(usage) != 1 {
		t.Fatalf("usage reports = %d, want 1", len(usage))
	}
	want := inference.Usage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17}
	if !reflect.DeepEqual(usage[0], want) {
		t.Fatalf("usage = %+v, want %+v", usage[0], want)
	}
}

func TestConcurrentCloseReturnsSameAggregateAndResultLast(t *testing.T) {
	var mu sync.Mutex
	var log []string
	busErr := errors.New("bus close failed")
	integrationErr := errors.New("integration close failed")
	bus := &trackedBus{
		MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu, closeErr: busErr,
	}
	builder := NewBuilder(newDeployBuilder(t, bus, &struct{}{}, nil))
	factory := &testIntegrationFactory{
		spec: IntegrationSpec{Kind: "lifecycle"},
		log:  &log, mu: &mu, closeErr: integrationErr,
	}
	registerLifecycle(t, builder, factory)
	runtime, err := builder.Build(context.Background(), runtimeDoc(t,
		"  integrations:\n    - {name: a, kind: lifecycle}\n"))
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
			errs <- runtime.Close()
		}()
	}
	wait.Wait()
	close(errs)
	var first error
	for closeErr := range errs {
		if !errors.Is(closeErr, busErr) || !errors.Is(closeErr, integrationErr) {
			t.Fatalf("Close error lost aggregate members: %v", closeErr)
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
	if indexOf(got, "close.a") > indexOf(got, "result.close") {
		t.Fatalf("result was not closed last: %v", got)
	}
	if _, err := runtime.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "after-close",
	}); !errors.Is(err, session.ErrManagerClosed) {
		t.Fatalf("manager accepted work after close: %v", err)
	}
}

func TestBuilderIsSingleUse(t *testing.T) {
	var mu sync.Mutex
	var log []string
	bus := &trackedBus{MemoryBus: event.NewMemoryBus(), log: &log, mu: &mu}
	builder := NewBuilder(newDeployBuilder(t, bus, &struct{}{}, nil))
	runtime, err := builder.Build(context.Background(), runtimeDoc(t, ""))
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	if runtime.Scheduler() != nil {
		t.Fatalf("Scheduler() = %p, want nil when runtime.scheduler is omitted", runtime.Scheduler())
	}
	if _, err := builder.Build(context.Background(), runtimeDoc(t, "")); err == nil ||
		!strings.Contains(err.Error(), "already been used") {
		t.Fatalf("second Build error = %v", err)
	}
}

func TestBaseHostPublishesAndExposesBorrowedBus(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	factory, err := newBaseHostFactory(bus)
	if err != nil {
		t.Fatal(err)
	}
	interrupts := make(chan agent.Interrupt, 1)
	request := session.HostRequest{
		Key: session.Key{AgentID: "bot", ContextID: "ctx"}, RunID: "run",
		Interrupts: interrupts,
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, nil
		},
	}
	first, err := factory.NewHost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory.NewHost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(first).Pointer() == reflect.ValueOf(second).Pointer() {
		t.Fatal("base factory reused a Host across turns")
	}
	provider, ok := first.(agent.EventBusProvider)
	if !ok || provider.EventBus() != bus {
		t.Fatal("base Host did not expose the borrowed event bus")
	}
	sub, err := bus.Subscribe(context.Background(), "runtime.test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	envelope, err := event.NewEnvelope(context.Background(), "runtime.test", map[string]string{"ok": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.C():
	case <-time.After(time.Second):
		t.Fatal("published envelope did not reach explicit bus")
	}
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}
