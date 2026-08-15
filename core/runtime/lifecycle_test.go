package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

// simpleRunEngine publishes the run lifecycle delimiters the stream
// coordinator needs and completes immediately.
func simpleRunEngine() agent.Engine {
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run); err != nil {
			return board, err
		}
		if err := publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run); err != nil {
			return board, err
		}
		return board, nil
	})
}

// blockingRunEngine holds a turn open until release is closed.
func blockingRunEngine(release <-chan struct{}) agent.Engine {
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run); err != nil {
			return board, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return board, ctx.Err()
		}
		if err := publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run); err != nil {
			return board, err
		}
		return board, nil
	})
}

func lifecycleDoc(t *testing.T) deploy.Document {
	t.Helper()
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: test}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
`)
}

func buildLifecycleApp(
	t *testing.T,
	doc deploy.Document,
	engine agent.Engine,
) (*Runtime, event.Bus) {
	t.Helper()
	bus := event.NewMemoryBus()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: engine})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: bus,
	})
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app, bus
}

func dynamicDefinition(name string) agent.Definition {
	return agent.Definition{
		Card:   agent.AgentCard{Name: name},
		Engine: agent.EngineRef{Kind: testEngineKind, Impl: testEngineImpl},
	}
}

func TestRuntimeRegisterAgentEndToEnd(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	instance, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn"))
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if instance.ID != "dyn" {
		t.Fatalf("instance ID = %q, want dyn", instance.ID)
	}
	if got, ok := app.Agent("dyn"); !ok || got != instance {
		t.Fatal("Agent(dyn) did not resolve the registered instance")
	}
	names := app.AgentNames()
	if len(names) != 2 || names[0] != "bot" || names[1] != "dyn" {
		t.Fatalf("AgentNames = %v, want [bot dyn]", names)
	}

	result := runTurn(t, app.Sessions(), "dyn", "conv")
	if result.Status != agent.StatusCompleted {
		t.Fatalf("dynamic agent turn status = %q", result.Status)
	}
}

func TestRuntimeRegisterAgentConflicts(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	if _, err := app.RegisterAgent(
		context.Background(), "bot", dynamicDefinition("Bot")); !errdefs.IsConflict(err) {
		t.Fatalf("RegisterAgent over deployed name error = %v, want conflict", err)
	}
	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("first RegisterAgent: %v", err)
	}
	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn2")); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate RegisterAgent error = %v, want conflict", err)
	}
}

func TestRuntimeRegisterAgentRejectsInvalidInput(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	if _, err := app.RegisterAgent(
		context.Background(), " bad", dynamicDefinition("Bad")); !errdefs.IsValidation(err) {
		t.Fatalf("padded id error = %v, want validation", err)
	}
	bad := dynamicDefinition("Bad")
	bad.Card = agent.AgentCard{} // missing name
	if _, err := app.RegisterAgent(
		context.Background(), "bad", bad); !errdefs.IsValidation(err) {
		t.Fatalf("invalid definition error = %v, want validation", err)
	}
	if _, err := app.RegisterAgent(
		context.Background(), "bad", dynamicDefinition("Bad"),
		WithToolAssembly("   ")); !errdefs.IsValidation(err) {
		t.Fatalf("blank tool resource error = %v, want validation", err)
	}
}

func TestRuntimeUnregisterAgent(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if err := app.UnregisterAgent(context.Background(), "dyn"); err != nil {
		t.Fatalf("UnregisterAgent: %v", err)
	}
	if _, ok := app.Agent("dyn"); ok {
		t.Fatal("Agent(dyn) still resolves after removal")
	}
	names := app.AgentNames()
	if len(names) != 1 || names[0] != "bot" {
		t.Fatalf("AgentNames after removal = %v, want [bot]", names)
	}
	if _, err := app.Sessions().Open(
		context.Background(),
		keyFor("dyn", "conv"),
	); !errdefs.IsNotFound(err) {
		t.Fatalf("Open removed agent error = %v, want not found", err)
	}

	// Unknown name is an idempotent no-op; deployed agents conflict.
	if err := app.UnregisterAgent(context.Background(), "nope"); err != nil {
		t.Fatalf("unknown UnregisterAgent error = %v, want nil", err)
	}
	if err := app.UnregisterAgent(context.Background(), "bot"); !errdefs.IsConflict(err) {
		t.Fatalf("UnregisterAgent deployed error = %v, want conflict", err)
	}
}

func TestRuntimeReRegisterAfterRemoval(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	for i := 0; i < 2; i++ {
		if _, err := app.RegisterAgent(
			context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
			t.Fatalf("RegisterAgent round %d: %v", i, err)
		}
		if result := runTurn(t, app.Sessions(), "dyn", "conv"); result.Status != agent.StatusCompleted {
			t.Fatalf("round %d status = %q", i, result.Status)
		}
		if err := app.UnregisterAgent(context.Background(), "dyn"); err != nil {
			t.Fatalf("UnregisterAgent round %d: %v", i, err)
		}
	}
}

func TestRuntimeUnregisterAgentDrainTimeout(t *testing.T) {
	release := make(chan struct{})
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), blockingRunEngine(release))

	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	lease, err := app.Sessions().GetOrCreate(context.Background(), keyFor("dyn", "conv"))
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = app.UnregisterAgent(
		context.Background(), "dyn", WithRemoveTimeout(50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("UnregisterAgent error = %v, want DeadlineExceeded", err)
	}
	// Registration restored, sessions blocked, agent still present.
	if _, ok := app.Agent("dyn"); !ok {
		t.Fatal("agent disappeared after failed removal")
	}
	if _, err := app.Sessions().Open(
		context.Background(),
		keyFor("dyn", "conv2"),
	); !errdefs.IsNotFound(err) {
		t.Fatalf("Open after failed removal error = %v, want not found", err)
	}

	close(release)
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	if err := app.UnregisterAgent(context.Background(), "dyn"); err != nil {
		t.Fatalf("retry UnregisterAgent: %v", err)
	}
	if _, ok := app.Agent("dyn"); ok {
		t.Fatal("agent still present after successful removal")
	}
}

func TestRuntimeRegisterAgentToolAssembly(t *testing.T) {
	got := make(chan []string, 1)
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: catalogEngine(t, got)})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: event.NewMemoryBus(),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "yaml"},
		value: buildTestAssembly(t, "research_tool"),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "test"},
		value: buildTestAssembly(t, "assist_tool"),
	})
	doc := dynamicCatalogDoc(t, `      researcher: research_tools
      assistant: assistant_tools
`)
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn"),
		WithToolAssembly("assistant_tools"),
	); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if result := runTurn(t, app.Sessions(), "dyn", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("turn status = %q", result.Status)
	}
	select {
	case defs := <-got:
		assertDefinitions(t, defs, []string{"assist_tool"}, []string{"research_tool"})
	case <-time.After(5 * time.Second):
		t.Fatal("dynamic agent turn never observed its tool catalog")
	}

	if _, err := app.RegisterAgent(
		context.Background(), "dyn2", dynamicDefinition("Dyn2"),
		WithToolAssembly("missing_tools"),
	); !errdefs.IsNotFound(err) {
		t.Fatalf("missing tool resource error = %v, want not found", err)
	}
	if _, ok := app.Agent("dyn2"); ok {
		t.Fatal("agent registered despite missing tool resource")
	}
	// No default configured: an agent without an explicit assembly is
	// rejected up front (mirrors the build-time rule).
	if _, err := app.RegisterAgent(
		context.Background(), "dyn3", dynamicDefinition("Dyn3"),
	); !errdefs.IsValidation(err) {
		t.Fatalf("register without assembly and no default error = %v, want validation", err)
	}
	if _, ok := app.Agent("dyn3"); ok {
		t.Fatal("agent registered despite missing tool mapping")
	}
}

func TestRuntimeRegisterAgentWithoutDynamicCatalog(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn"),
		WithToolAssembly("kit"),
	); !errdefs.IsValidation(err) {
		t.Fatalf("tool assembly without dynamic catalog error = %v, want validation", err)
	}
	// Without an explicit assembly the agent still registers and runs.
	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent without dynamic catalog: %v", err)
	}
}

func TestRuntimeRegisterAfterClose(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("RegisterAgent after Close error = %v, want not available", err)
	}
	if err := app.UnregisterAgent(context.Background(), "bot"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("UnregisterAgent after Close error = %v, want not available", err)
	}
}

func TestRuntimeLifecycleEvents(t *testing.T) {
	app, bus := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())
	sub, err := bus.Subscribe(context.Background(), PatternAgentLifecycle())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := app.RegisterAgent(
		context.Background(), "dyn", dynamicDefinition("Dyn")); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if err := app.UnregisterAgent(context.Background(), "dyn"); err != nil {
		t.Fatalf("UnregisterAgent: %v", err)
	}

	var subjects []event.Subject
	deadline := time.After(5 * time.Second)
	for len(subjects) < 2 {
		select {
		case env := <-sub.C():
			subjects = append(subjects, env.Subject)
			var payload AgentLifecycleEvent
			if err := env.Decode(&payload); err != nil {
				t.Fatalf("decode lifecycle payload: %v", err)
			}
			if payload.AgentID != "dyn" {
				t.Fatalf("payload agent_id = %q, want dyn", payload.AgentID)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for lifecycle events, got %v", subjects)
		}
	}
	if subjects[0] != SubjectAgentRegistered("dyn") ||
		subjects[1] != SubjectAgentRemoved("dyn") {
		t.Fatalf("subjects = %v, want [registered removed]", subjects)
	}
}

func TestRuntimeRegisterUnregisterConcurrent(t *testing.T) {
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), simpleRunEngine())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = app.RegisterAgent(
					context.Background(), "dyn", dynamicDefinition("Dyn"))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = app.UnregisterAgent(context.Background(), "dyn")
			}
		}()
	}
	wg.Wait()

	// Whatever the final state, the runtime must stay internally
	// consistent: a removed agent refuses new sessions; a registered one
	// resolves.
	if _, ok := app.Agent("dyn"); ok {
		if result := runTurn(t, app.Sessions(), "dyn", "conv"); result.Status != agent.StatusCompleted {
			t.Fatalf("final registered agent turn status = %q", result.Status)
		}
	}
}

func keyFor(agentID, contextID string) session.Key {
	return session.Key{AgentID: agentID, ContextID: contextID}
}
