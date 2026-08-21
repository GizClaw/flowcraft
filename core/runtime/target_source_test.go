package runtime

import (
	"context"
	"testing"

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
