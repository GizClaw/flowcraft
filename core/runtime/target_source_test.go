package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

func directoryDoc(t *testing.T, extra string) deploy.Document {
	t.Helper()
	doc := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  directory: {kind: delegation.Directory, impl: local}
`+extra+`
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
`)
	return doc
}

func buildDirectoryApp(t *testing.T, doc deploy.Document) *Runtime {
	t.Helper()
	bus := event.NewMemoryBus()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: simpleRunEngine()})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: bus,
	})
	reg.MustRegister(delegation.NewDirectoryFactory())
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func TestRuntimeRegisterAgentBecomesDelegationTarget(t *testing.T) {
	app := buildDirectoryApp(t, directoryDoc(t, ""))
	value, ok := app.Resource("directory")
	directory, typeOK := value.(*delegation.LocalDirectory)
	if !ok || !typeOK || directory == nil {
		t.Fatalf("directory resource = %v, want *delegation.LocalDirectory", value)
	}

	ctx := context.Background()
	if _, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	targets, err := directory.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != "bot" || targets[1].ID != "dyn" {
		t.Fatalf("List = %+v, want [bot dyn]", targets)
	}
	if _, err := directory.Lookup(ctx, "dyn"); err != nil {
		t.Fatalf("Lookup(dyn): %v", err)
	}

	if err := app.UnregisterAgent(ctx, "dyn"); err != nil {
		t.Fatalf("UnregisterAgent: %v", err)
	}
	targets, err = directory.List(ctx)
	if err != nil {
		t.Fatalf("List after unregister: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != "bot" {
		t.Fatalf("List after unregister = %+v, want [bot]", targets)
	}
	if _, err := directory.Lookup(ctx, "dyn"); !errdefs.IsNotFound(err) {
		t.Fatalf("Lookup(dyn) after unregister error = %v, want not found", err)
	}
}

func TestRuntimeMultipleTargetViewBindersConflict(t *testing.T) {
	doc := directoryDoc(t, `
  directory2: {kind: delegation.Directory, impl: local}
`)
	bus := event.NewMemoryBus()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: simpleRunEngine()})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: bus,
	})
	reg.MustRegister(delegation.NewDirectoryFactory())
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err == nil {
		_ = app.Close()
		t.Fatal("Build with two target view binders succeeded, want conflict")
	}
	if !errdefs.IsConflict(err) {
		t.Fatalf("Build error = %v, want conflict", err)
	}
}

func TestFreezeTargetViewsPinsDynamicSet(t *testing.T) {
	app := buildDirectoryApp(t, directoryDoc(t, ""))
	value, ok := app.Resource("directory")
	directory, typeOK := value.(*delegation.LocalDirectory)
	if !ok || !typeOK {
		t.Fatalf("directory resource = %v, want *delegation.LocalDirectory", value)
	}

	ctx := context.Background()
	instance, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn"))
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	// Pin the dynamic set exactly as generation retirement would.
	freezeTargetViews(app.current.result, map[string]*agent.Agent{"dyn": instance})

	// A registration made after the freeze must not appear in the
	// retired generation's directory.
	if _, err := app.RegisterAgent(ctx, "zeta", dynamicDefinition("Zeta")); err != nil {
		t.Fatalf("RegisterAgent(zeta): %v", err)
	}
	targets, err := directory.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != "bot" || targets[1].ID != "dyn" {
		t.Fatalf("List after freeze = %+v, want [bot dyn]", targets)
	}
	if _, err := directory.Lookup(ctx, "dyn"); err != nil {
		t.Fatalf("Lookup(dyn) after freeze: %v", err)
	}
	if _, err := directory.Lookup(ctx, "zeta"); !errdefs.IsNotFound(err) {
		t.Fatalf("Lookup(zeta) after freeze error = %v, want not found", err)
	}
}

func TestFreezeUnfreezeTargetViewsRoundTrip(t *testing.T) {
	app := buildDirectoryApp(t, directoryDoc(t, ""))
	value, ok := app.Resource("directory")
	directory, typeOK := value.(*delegation.LocalDirectory)
	if !ok || !typeOK {
		t.Fatalf("directory resource = %v, want *delegation.LocalDirectory", value)
	}

	ctx := context.Background()
	instance, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn"))
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	// Simulate a freeze at the swap point, then the rollback path.
	freezeTargetViews(app.current.result, map[string]*agent.Agent{"dyn": instance})
	unfreezeTargetViews(app.current.result)

	if _, err := app.RegisterAgent(ctx, "zeta", dynamicDefinition("Zeta")); err != nil {
		t.Fatalf("RegisterAgent(zeta) after unfreeze: %v", err)
	}
	targets, err := directory.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 3 || targets[0].ID != "bot" ||
		targets[1].ID != "dyn" || targets[2].ID != "zeta" {
		t.Fatalf("List after unfreeze = %+v, want [bot dyn zeta]", targets)
	}
	if _, err := directory.Lookup(ctx, "zeta"); err != nil {
		t.Fatalf("Lookup(zeta) after unfreeze: %v", err)
	}
}

func TestRuntimeReloadRetiredDirectoryFrozen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: simpleRunEngine()})
	reg.MustRegister(freshBusFactory{})
	reg.MustRegister(delegation.NewDirectoryFactory())
	app, err := NewBuilder(reg).Build(ctx, directoryDoc(t, ""))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if _, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	oldValue, _ := app.current.result.Value("directory")
	oldDirectory, ok := oldValue.(*delegation.LocalDirectory)
	if !ok {
		t.Fatalf("old directory resource = %T", oldValue)
	}

	if _, err := app.Reload(ctx, directoryDoc(t, "")); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	newValue, ok := app.Resource("directory")
	newDirectory, typeOK := newValue.(*delegation.LocalDirectory)
	if !ok || !typeOK {
		t.Fatalf("new directory resource = %v, want *delegation.LocalDirectory", newValue)
	}

	// A registration after the swap is visible to the new generation
	// only; the retired directory stays pinned to the frozen set.
	if _, err := app.RegisterAgent(ctx, "zeta", dynamicDefinition("Zeta")); err != nil {
		t.Fatalf("RegisterAgent(zeta): %v", err)
	}
	oldTargets, err := oldDirectory.List(ctx)
	if err != nil {
		t.Fatalf("old List: %v", err)
	}
	if len(oldTargets) != 2 || oldTargets[0].ID != "bot" || oldTargets[1].ID != "dyn" {
		t.Fatalf("old List after reload = %+v, want [bot dyn]", oldTargets)
	}
	if _, err := oldDirectory.Lookup(ctx, "zeta"); !errdefs.IsNotFound(err) {
		t.Fatalf("old Lookup(zeta) error = %v, want not found", err)
	}
	if _, err := oldDirectory.Lookup(ctx, "dyn"); err != nil {
		t.Fatalf("old Lookup(dyn) (in-flight) : %v", err)
	}

	newTargets, err := newDirectory.List(ctx)
	if err != nil {
		t.Fatalf("new List: %v", err)
	}
	if len(newTargets) != 3 || newTargets[1].ID != "dyn" || newTargets[2].ID != "zeta" {
		t.Fatalf("new List after reload = %+v, want [bot dyn zeta]", newTargets)
	}
}

// failingBusFactory builds a working bus for the first generation and a
// closed bus (Subscribe always fails) for every later generation, so a
// reload's router.AddBus fails after the swap-point freeze — the one
// rollback path reachable through the public API.
type failingBusFactory struct {
	built atomic.Int64
}

func (*failingBusFactory) Spec() resource.Spec {
	return resource.Spec{Kind: testEventKind, Impl: testEventImpl}
}

func (f *failingBusFactory) New(context.Context, resource.Input) (any, error) {
	if f.built.Add(1) == 1 {
		return event.NewMemoryBus(), nil
	}
	bus := event.NewMemoryBus()
	_ = bus.Close()
	return bus, nil
}

func TestRuntimeReloadRollbackUnfreezesDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: simpleRunEngine()})
	reg.MustRegister(&failingBusFactory{})
	reg.MustRegister(delegation.NewDirectoryFactory())
	app, err := NewBuilder(reg).Build(ctx, directoryDoc(t, ""))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	value, ok := app.Resource("directory")
	directory, typeOK := value.(*delegation.LocalDirectory)
	if !ok || !typeOK {
		t.Fatalf("directory resource = %v, want *delegation.LocalDirectory", value)
	}
	if _, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	// Give the router an attachment so AddBus actually subscribes on
	// the new generation's bus and hits the closed-bus failure.
	if _, err := app.router.Attach(ctx, event.Pattern("test.>"),
		event.SinkFunc(func(context.Context, event.Envelope) error { return nil })); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The new generation's bus cannot be subscribed, so AddBus fails
	// after the swap-point freeze and must roll the directory back to
	// the live view.
	if _, reloadErr := app.Reload(ctx, directoryDoc(t, "")); reloadErr == nil {
		t.Fatal("Reload with failing bus succeeded, want error")
	}
	if app.current.id != 1 {
		t.Fatalf("generation id after failed reload = %d, want 1", app.current.id)
	}

	if _, err := app.RegisterAgent(ctx, "zeta", dynamicDefinition("Zeta")); err != nil {
		t.Fatalf("RegisterAgent(zeta) after failed reload: %v", err)
	}
	targets, err := directory.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 3 || targets[0].ID != "bot" ||
		targets[1].ID != "dyn" || targets[2].ID != "zeta" {
		t.Fatalf("List after failed reload = %+v, want [bot dyn zeta]", targets)
	}
	if _, err := directory.Lookup(ctx, "zeta"); err != nil {
		t.Fatalf("Lookup(zeta) after failed reload: %v", err)
	}
}

func TestRuntimeDynamicAgentDelegateEndToEnd(t *testing.T) {
	ctx := context.Background()
	reg := newBaseRegistry(t, event.NewMemoryBus(), &recordingCheckpointStore{}, noopEngine())
	reg.MustRegister(delegation.NewDirectoryFactory())
	reg.MustRegister(delegation.NewServiceFactory())
	doc := baseRuntimeDoc(t)
	doc.Resources["delegation_directory"] = resource.Resource{
		Kind: delegation.DirectoryKind, Impl: "local",
	}
	doc.Resources["delegation"] = resource.Resource{
		Kind: delegation.ServiceKind, Impl: "local",
		Deps: resource.Deps{"directory": "delegation_directory"},
	}
	app, err := NewBuilder(reg).Build(ctx, doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	serviceValue, _ := app.Resource("delegation")
	service, ok := serviceValue.(*delegation.LocalService)
	if !ok {
		t.Fatalf("delegation resource = %T, want *delegation.LocalService", serviceValue)
	}

	if _, err := app.RegisterAgent(ctx, "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	response, err := service.Delegate(ctx, delegation.Request{
		Mode:   delegation.ModeSync,
		Target: "dyn",
		Input:  "do it",
	})
	if err != nil {
		t.Fatalf("Delegate to dynamic agent: %v", err)
	}
	if response.Status != delegation.StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}

	if err := app.UnregisterAgent(ctx, "dyn"); err != nil {
		t.Fatalf("UnregisterAgent: %v", err)
	}
	if _, err := service.Delegate(ctx, delegation.Request{
		Mode:   delegation.ModeSync,
		Target: "dyn",
		Input:  "do it again",
	}); !errdefs.IsNotFound(err) {
		t.Fatalf("Delegate after unregister error = %v, want not found", err)
	}
}

func TestDirectoryConcurrentSourceAccess(t *testing.T) {
	app := buildDirectoryApp(t, directoryDoc(t, ""))
	value, ok := app.Resource("directory")
	directory, typeOK := value.(*delegation.LocalDirectory)
	if !ok || !typeOK {
		t.Fatalf("directory resource = %v, want *delegation.LocalDirectory", value)
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := fmt.Sprintf("dyn-%d", n)
			if _, err := app.RegisterAgent(ctx, name, dynamicDefinition(name)); err != nil {
				t.Errorf("RegisterAgent(%s): %v", name, err)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = directory.List(ctx)
			_, _ = directory.Get(ctx, "bot")
			_, _ = directory.Lookup(ctx, "dyn-3")
		}()
	}
	wg.Wait()

	targets, err := directory.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 9 {
		t.Fatalf("List = %d targets, want 9 (bot + 8 dynamic)", len(targets))
	}
}
