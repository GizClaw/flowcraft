package memory

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/GizClaw/flowcraft/sdkx/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdkx/scheduler/config"
	memoryscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler/memory"
)

type resourceFactory struct {
	spec  deploy.ResourceSpec
	value any
}

func (f resourceFactory) Spec() deploy.ResourceSpec { return f.spec }
func (f resourceFactory) New(context.Context, deploy.ResourceInput) (any, error) {
	return f.value, nil
}

type remoteSchedulerServer struct {
	sdkscheduler.Server
	startErr error
}

func (s *remoteSchedulerServer) Start() error {
	return s.startErr
}

type failingIntegrationFactory struct {
	phase string
}

func (*failingIntegrationFactory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{Kind: "test.failure"}
}

func (f *failingIntegrationFactory) Prepare(
	context.Context,
	runtimecore.PrepareInput,
) (runtimecore.PreparedIntegration, error) {
	return &failingIntegration{phase: f.phase}, nil
}

type failingIntegration struct {
	phase string
}

func (i *failingIntegration) Bind(context.Context, runtimecore.BindInput) error {
	if i.phase == "bind" {
		return errors.New("bind failed")
	}
	return nil
}

func (*failingIntegration) DecorateHost(
	base session.HostFactory,
) (session.HostFactory, error) {
	return base, nil
}

func (i *failingIntegration) Start(context.Context) error {
	if i.phase == "start" {
		return errors.New("start failed")
	}
	return nil
}

func (*failingIntegration) Close() error { return nil }

func memorySettings(t *testing.T, yaml string) *deploy.Opaque {
	t.Helper()
	doc, err := deploy.Parse([]byte(`
version: v1
runtime:
  event_bus: events
  scheduler: schedules
  integrations:
    - name: memory
      kind: memory.maintenance
      deps: {memory: memories}
      settings: ` + yaml + `
`))
	if err != nil {
		t.Fatal(err)
	}
	config, err := runtimecore.DecodeConfig(doc)
	if err != nil {
		t.Fatal(err)
	}
	return config.Integrations[0].Settings
}

func TestFactorySpec(t *testing.T) {
	spec := NewFactory().Spec()
	if spec.Kind != Kind || len(spec.Deps) != 2 {
		t.Fatalf("Spec = %+v", spec)
	}
	memoryDep := spec.Deps[0]
	if memoryDep.Name != "memory" || memoryDep.Kind != "memory.Assembly" || !memoryDep.Required ||
		memoryDep.Type != reflect.TypeFor[*memoryconfig.Assembly]() {
		t.Fatalf("memory dependency = %+v", memoryDep)
	}
	schedulerDep := spec.Deps[1]
	if schedulerDep.Name != "scheduler" || schedulerDep.Kind != schedulerconfig.ResourceKind ||
		!schedulerDep.Required || schedulerDep.Type != reflect.TypeFor[sdkscheduler.Server]() {
		t.Fatalf("scheduler dependency = %+v", schedulerDep)
	}
}

func TestPrepareSettingsMustBeEmpty(t *testing.T) {
	factory := NewFactory()
	for _, test := range []struct {
		name     string
		settings *deploy.Opaque
		wantErr  bool
	}{
		{name: "omitted"},
		{name: "empty", settings: memorySettings(t, "{}")},
		{name: "unknown", settings: memorySettings(t, "{unexpected: true}"), wantErr: true},
		{name: "non-map", settings: memorySettings(t, "enabled"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
				Name: "memory", Kind: Kind, Settings: test.settings,
			})
			if test.wantErr {
				if !errdefs.IsValidation(err) {
					t.Fatalf("Prepare error = %v, want validation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloseDoesNotCloseBorrowedAssemblyOrSchedulerService(t *testing.T) {
	runtime, err := sdkmemory.New(sdkmemory.Spec{RuntimeID: "test"}, sdkmemory.Impls{})
	if err != nil {
		t.Fatal(err)
	}
	var assemblyCloses atomic.Int32
	runtime.RegisterClose(func() error {
		assemblyCloses.Add(1)
		return nil
	})
	assembly := &memoryconfig.Assembly{Runtime: runtime}
	server, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := memoryscheduler.New(
		context.Background(), server, "memory", assembly.Runtime, assembly.Lifecycle,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &integration{name: "memory", bound: true, scheduler: adapter}
	if err := prepared.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if got := assemblyCloses.Load(); got != 0 {
		t.Fatalf("integration closed borrowed assembly %d times", got)
	}
	other, err := memoryscheduler.New(
		context.Background(), server, "other", assembly.Runtime, assembly.Lifecycle,
	)
	if err != nil {
		t.Fatalf("mount after integration Close: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := prepared.Start(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start after Close error = %v, want not available", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatal(err)
	}
	if got := assemblyCloses.Load(); got != 1 {
		t.Fatalf("assembly close count = %d, want 1", got)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedRuntimeBuildRestoresRemoteSchedulerRules(t *testing.T) {
	for _, test := range []struct {
		name          string
		integration   string
		schedulerFail bool
	}{
		{name: "later bind failure", integration: "bind"},
		{name: "later integration start failure", integration: "start"},
		{name: "scheduler start failure", schedulerFail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, original := newRemoteSchedulerWithOriginalRule(t)
			if test.schedulerFail {
				server.startErr = errors.New("scheduler start failed")
			}
			builder, document := memoryRuntimeBuilder(
				t,
				server,
				test.integration,
				true,
			)

			if _, err := builder.Build(context.Background(), document); err == nil {
				t.Fatal("Build succeeded, want failure")
			}
			assertRulesMatchOriginal(t, server.Server, original)
		})
	}
}

func TestSuccessfulRuntimeClosePreservesConfiguredRemoteSchedulerRules(t *testing.T) {
	server, _ := newRemoteSchedulerWithOriginalRule(t)
	builder, document := memoryRuntimeBuilder(t, server, "", false)

	app, err := builder.Build(context.Background(), document)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Runtime.Close: %v", err)
	}
	rules, err := server.ListRules(context.Background(), "memory")
	if err != nil {
		t.Fatalf("ListRules after Runtime.Close: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules after Runtime.Close = %+v, want configured compact and archive", rules)
	}
	byID := make(map[string]sdkscheduler.Rule, len(rules))
	for _, rule := range rules {
		byID[rule.ID] = rule
	}
	if byID[memoryscheduler.CompactRuleID].Cron != "*/5 * * * *" ||
		byID[memoryscheduler.ArchiveRuleID].Cron != "0 * * * *" {
		t.Fatalf("configured rules were not preserved: %+v", rules)
	}
}

func newRemoteSchedulerWithOriginalRule(
	t *testing.T,
) (*remoteSchedulerServer, sdkscheduler.Rule) {
	t.Helper()
	local, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	server := &remoteSchedulerServer{Server: local}
	if _, ok := any(server).(interface{ Close() error }); ok {
		t.Fatal("remote scheduler fake unexpectedly implements io.Closer")
	}
	payload, err := sdkscheduler.NewJSONPayload(
		memoryscheduler.PayloadKind,
		memoryscheduler.PayloadVersion,
		memoryscheduler.Task{Compact: &memoryscheduler.CompactTask{
			OlderThan: 72 * time.Hour,
			Keep:      11,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	original := sdkscheduler.Rule{
		Namespace: "memory",
		ID:        memoryscheduler.CompactRuleID,
		Cron:      "17 3 * * *",
		Timezone:  "UTC",
		Overlap:   sdkscheduler.OverlapAllow,
		Task:      sdkscheduler.Task{Payload: payload},
	}
	if err := server.PutRule(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	return server, original
}

func memoryRuntimeBuilder(
	t *testing.T,
	server *remoteSchedulerServer,
	failurePhase string,
	includeFailure bool,
) (*runtimecore.Builder, deploy.Document) {
	t.Helper()
	memoryRuntime, err := sdkmemory.New(
		sdkmemory.Spec{RuntimeID: "test"},
		sdkmemory.Impls{},
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly := &memoryconfig.Assembly{
		Runtime: memoryRuntime,
		Lifecycle: memoryconfig.LifecycleSpec{
			Compact: memoryconfig.CompactLifecycleSpec{
				Cron: "*/5 * * * *", OlderThan: 24 * time.Hour, Keep: 3,
			},
			Archive: memoryconfig.ArchiveLifecycleSpec{
				Cron: "0 * * * *", OlderThan: 7 * 24 * time.Hour, Destination: "cold",
			},
		},
	}
	deployBuilder := deploy.NewBuilder(agent.NewRegistry())
	for _, factory := range []deploy.ResourceFactory{
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: "event.Bus", Impl: "test"},
			value: event.NewMemoryBus(),
		},
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: "memory.Assembly", Impl: "test"},
			value: assembly,
		},
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: schedulerconfig.ResourceKind, Impl: "test"},
			value: server,
		},
	} {
		if err := deployBuilder.RegisterResource(factory); err != nil {
			t.Fatal(err)
		}
	}
	builder := runtimecore.NewBuilder(deployBuilder)
	if err := builder.RegisterIntegration(NewFactory()); err != nil {
		t.Fatal(err)
	}
	integrations := `
    - name: memory
      kind: memory.maintenance
      deps: {memory: memories}
`
	if includeFailure {
		if err := builder.RegisterIntegration(&failingIntegrationFactory{
			phase: failurePhase,
		}); err != nil {
			t.Fatal(err)
		}
		integrations += "    - {name: failure, kind: test.failure}\n"
	}
	document, err := deploy.Parse([]byte(`
version: v1
resources:
  events: {kind: event.Bus, impl: test}
  memories: {kind: memory.Assembly, impl: test}
  schedules: {kind: scheduler.Server, impl: test}
agents: {}
runtime:
  event_bus: events
  scheduler: schedules
  integrations:` + integrations))
	if err != nil {
		t.Fatal(err)
	}
	return builder, document
}

func assertRulesMatchOriginal(
	t *testing.T,
	server sdkscheduler.Server,
	original sdkscheduler.Rule,
) {
	t.Helper()
	rules, err := server.ListRules(context.Background(), original.Namespace)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules after failed Build = %+v, want only original", rules)
	}
	got := rules[0]
	if got.ID != original.ID || got.Cron != original.Cron ||
		got.Timezone != original.Timezone || got.Overlap != original.Overlap ||
		!reflect.DeepEqual(got.Task.Payload, original.Task.Payload) {
		t.Fatalf("restored rule = %+v, want %+v", got, original)
	}
}

func TestRuntimeBindsMemoryAssemblyAndClosesItAfterIntegration(t *testing.T) {
	for _, test := range []struct {
		name      string
		assembly  *memoryconfig.Assembly
		wantError bool
	}{
		{
			name: "valid",
			assembly: func() *memoryconfig.Assembly {
				runtime, err := sdkmemory.New(sdkmemory.Spec{RuntimeID: "test"}, sdkmemory.Impls{})
				if err != nil {
					t.Fatal(err)
				}
				return &memoryconfig.Assembly{Runtime: runtime}
			}(),
		},
		{name: "nil runtime", assembly: &memoryconfig.Assembly{}, wantError: true},
		{name: "typed nil assembly", assembly: nil, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := event.NewMemoryBus()
			server, err := scheduler.NewLocalServer()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			deployBuilder := deploy.NewBuilder(agent.NewRegistry())
			if err := deployBuilder.RegisterResource(resourceFactory{
				spec:  deploy.ResourceSpec{Kind: "event.Bus", Impl: "test"},
				value: bus,
			}); err != nil {
				t.Fatal(err)
			}
			if err := deployBuilder.RegisterResource(resourceFactory{
				spec:  deploy.ResourceSpec{Kind: "memory.Assembly", Impl: "test"},
				value: test.assembly,
			}); err != nil {
				t.Fatal(err)
			}
			if err := deployBuilder.RegisterResource(resourceFactory{
				spec:  deploy.ResourceSpec{Kind: schedulerconfig.ResourceKind, Impl: "test"},
				value: server,
			}); err != nil {
				t.Fatal(err)
			}
			builder := runtimecore.NewBuilder(deployBuilder)
			if err := builder.RegisterIntegration(NewFactory()); err != nil {
				t.Fatal(err)
			}
			document, err := deploy.Parse([]byte(`
version: v1
resources:
  events: {kind: event.Bus, impl: test}
  memories: {kind: memory.Assembly, impl: test}
  schedules: {kind: scheduler.Server, impl: test}
runtime:
  event_bus: events
  scheduler: schedules
  integrations:
    - name: memory
      kind: memory.maintenance
      deps: {memory: memories}
`))
			if err != nil {
				t.Fatal(err)
			}
			application, err := builder.Build(context.Background(), document)
			if test.wantError {
				if err == nil {
					_ = application.Close()
					t.Fatal("Build succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := application.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := server.ListRules(context.Background(), "after-runtime-close"); !errdefs.IsNotAvailable(err) {
				t.Fatalf("scheduler after Runtime.Close error = %v, want not available", err)
			}
		})
	}
}

type compactSpy struct {
	called chan sdkmemory.CompactRequest
}

func (*compactSpy) CompileCompact(
	context.Context,
	sdkmemory.CompactRequest,
) sdkmemory.CompileResult {
	return sdkmemory.CompileResult{
		Op: sdkmemory.OpCompact,
		Decisions: []sdkmemory.Decision{
			{Field: sdkmemory.FieldCompactScope, Disposition: sdkmemory.DispositionNative},
			{Field: sdkmemory.FieldCompactOlderThan, Disposition: sdkmemory.DispositionNative},
			{Field: sdkmemory.FieldCompactKeep, Disposition: sdkmemory.DispositionNative},
		},
	}
}

func (s *compactSpy) ExecuteCompact(
	_ context.Context,
	request sdkmemory.CompactRequest,
) (sdkmemory.CompactResponse, error) {
	s.called <- request
	return sdkmemory.CompactResponse{}, nil
}

func TestRuntimeMemoryIntegrationExecutesMaintenance(t *testing.T) {
	spy := &compactSpy{called: make(chan sdkmemory.CompactRequest, 1)}
	memoryRuntime, err := sdkmemory.New(sdkmemory.Spec{
		RuntimeID: "test",
		DefaultScope: sdkmemory.Scope{
			RuntimeID: "test",
			UserID:    "tenant",
		},
	}, sdkmemory.Impls{Compact: spy})
	if err != nil {
		t.Fatal(err)
	}
	assembly := &memoryconfig.Assembly{Runtime: memoryRuntime}
	server, err := scheduler.NewLocalServer()
	if err != nil {
		t.Fatal(err)
	}
	deployBuilder := deploy.NewBuilder(agent.NewRegistry())
	for _, factory := range []deploy.ResourceFactory{
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: "event.Bus", Impl: "test"},
			value: event.NewMemoryBus(),
		},
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: "memory.Assembly", Impl: "test"},
			value: assembly,
		},
		resourceFactory{
			spec:  deploy.ResourceSpec{Kind: schedulerconfig.ResourceKind, Impl: "test"},
			value: server,
		},
	} {
		if err := deployBuilder.RegisterResource(factory); err != nil {
			t.Fatal(err)
		}
	}
	builder := runtimecore.NewBuilder(deployBuilder)
	if err := builder.RegisterIntegration(NewFactory()); err != nil {
		t.Fatal(err)
	}
	document, err := deploy.Parse([]byte(`
version: v1
resources:
  events: {kind: event.Bus, impl: test}
  memories: {kind: memory.Assembly, impl: test}
  schedules: {kind: scheduler.Server, impl: test}
runtime:
  event_bus: events
  scheduler: schedules
  integrations:
    - name: memory
      kind: memory.maintenance
      deps: {memory: memories}
`))
	if err != nil {
		t.Fatal(err)
	}
	app, err := builder.Build(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sdkscheduler.NewClient[memoryscheduler.Task](
		app.Scheduler(), "memory", memoryscheduler.PayloadKind, memoryscheduler.PayloadVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.After(context.Background(), 0, memoryscheduler.Task{
		Compact: &memoryscheduler.CompactTask{OlderThan: time.Hour, Keep: 3},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-spy.called:
		if request.Scope.UserID != "tenant" || request.Keep != 3 {
			t.Fatalf("compact request = %+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance worker did not execute compact task")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}
