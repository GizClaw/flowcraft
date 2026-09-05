package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

type runtimeExternalPool struct {
	closes int
}

func (p *runtimeExternalPool) Close() error {
	p.closes++
	return nil
}

type runtimeExternalConsumer struct {
	pool   *runtimeExternalPool
	closed bool
}

func (c *runtimeExternalConsumer) Close() error {
	c.closed = true
	return nil
}

type runtimeExternalConsumerFactory struct{}

func (runtimeExternalConsumerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalConsumer",
		Impl: "runtime",
		Deps: []resource.DepSpec{{
			Name: "db", Type: "db.Pool", Required: true,
		}},
	}
}

func (runtimeExternalConsumerFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("db")
	if !ok {
		return nil, errdefs.Validationf("consumer: db dep missing")
	}
	pool, ok := value.(*runtimeExternalPool)
	if !ok {
		return nil, errdefs.Validationf("consumer: db dep has wrong type")
	}
	return &runtimeExternalConsumer{pool: pool}, nil
}

type runtimeExternalEngineFactory struct{}

func (runtimeExternalEngineFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalEngine",
		Impl: "runtime",
		Deps: []resource.DepSpec{{
			Name: "db", Type: "db.Pool", Required: true,
		}},
	}
}

func (runtimeExternalEngineFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("db")
	if !ok {
		return nil, errdefs.Validationf("engine: db dep missing")
	}
	if _, ok := value.(*runtimeExternalPool); !ok {
		return nil, errdefs.Validationf("engine: db dep has wrong type")
	}
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		_ = publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run)
		_ = publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run)
		return board, nil
	}), nil
}

func runtimeExternalDoc(t *testing.T, declare bool) deploy.Document {
	t.Helper()
	external := ""
	if declare {
		external = `  external_deps:
    - name: db
      contract: db.Pool
`
	}
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
  consumer:
    kind: test.ExternalConsumer
    impl: runtime
    deps:
      db: db
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
`+external)
}

func runtimeExternalBareDoc(t *testing.T) deploy.Document {
	t.Helper()
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
  external_deps:
    - name: db
      contract: db.Pool
`)
}

func newRuntimeExternalRegistry(t *testing.T) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: simpleRunEngine()})
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(runtimeExternalConsumerFactory{})
	reg.MustRegister(runtimeExternalEngineFactory{})
	return reg
}

func buildRuntimeExternal(
	t *testing.T,
	doc deploy.Document,
	pool *runtimeExternalPool,
) *Runtime {
	t.Helper()
	builder := NewBuilder(newRuntimeExternalRegistry(t))
	if err := builder.WithExternalResource(ExternalResource{
		ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"},
		Value:              pool,
	}); err != nil {
		t.Fatalf("WithExternalResource: %v", err)
	}
	app, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return app
}

func TestRuntimeExternalResourceInjectedAsDependencyAndUnmanaged(t *testing.T) {
	pool := &runtimeExternalPool{}
	app := buildRuntimeExternal(t, runtimeExternalDoc(t, true), pool)

	value, ok := app.Resource("consumer")
	if !ok {
		t.Fatalf("Resource(consumer) missing")
	}
	consumer := value.(*runtimeExternalConsumer)
	if consumer.pool != pool {
		t.Fatal("consumer did not receive the injected external pool")
	}
	if _, ok := app.Resource("db"); ok {
		t.Fatal("Resource(db) exposed the external dependency")
	}

	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times", pool.closes)
	}
	if !consumer.closed {
		t.Fatal("managed consumer was not closed")
	}
}

func TestRuntimeExternalResourceSurvivesReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := &runtimeExternalPool{}
	doc := runtimeExternalDoc(t, true)
	app := buildRuntimeExternal(t, doc, pool)

	if _, err := app.Reload(ctx, doc); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	value, ok := app.Resource("consumer")
	if !ok {
		t.Fatalf("Resource(consumer) missing after reload")
	}
	if consumer := value.(*runtimeExternalConsumer); consumer.pool != pool {
		t.Fatal("reloaded consumer did not receive the same external pool")
	}
	if _, ok := app.Resource("db"); ok {
		t.Fatal("Resource(db) exposed the external dependency after reload")
	}

	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times across reload", pool.closes)
	}
}

func TestRuntimeExternalResourceReloadCanDropDeclaration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := &runtimeExternalPool{}
	app := buildRuntimeExternal(t, runtimeExternalDoc(t, true), pool)

	dropDoc := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
`)
	if _, err := app.Reload(ctx, dropDoc); err != nil {
		t.Fatalf("Reload dropping external declaration: %v", err)
	}
	if _, ok := app.Resource("consumer"); ok {
		t.Fatal("old consumer leaked into the new generation")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times after declaration was dropped", pool.closes)
	}
}

func TestRuntimeExternalResourceDeclarationMismatch(t *testing.T) {
	t.Run("declared but not injected", func(t *testing.T) {
		builder := NewBuilder(newRuntimeExternalRegistry(t))
		_, err := builder.Build(context.Background(), runtimeExternalDoc(t, true))
		if err == nil || !strings.Contains(err.Error(), "declared but no external resource was injected") {
			t.Fatalf("Build error = %v, want missing injection", err)
		}
	})

	t.Run("injected but not declared", func(t *testing.T) {
		builder := NewBuilder(newRuntimeExternalRegistry(t))
		if err := builder.WithExternalResource(ExternalResource{
			ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"},
			Value:              &runtimeExternalPool{},
		}); err != nil {
			t.Fatalf("WithExternalResource: %v", err)
		}
		_, err := builder.Build(context.Background(), runtimeExternalDoc(t, false))
		if err == nil || !strings.Contains(err.Error(), "is not declared in runtime config") {
			t.Fatalf("Build error = %v, want undeclared injection", err)
		}
	})
}

func TestRuntimeExternalResourceReloadMissingInjectionRollsBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := &runtimeExternalPool{}
	app := buildRuntimeExternal(t, runtimeExternalDoc(t, true), pool)

	reloadDoc := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
  consumer:
    kind: test.ExternalConsumer
    impl: runtime
    deps:
      db: db
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1h, sink_buffer: 8}
  external_deps:
    - name: db
      contract: db.Pool
    - name: cache
      contract: cache.Client
`)
	if _, err := app.Reload(ctx, reloadDoc); err == nil ||
		!strings.Contains(err.Error(), "declared but no external resource was injected") {
		t.Fatalf("Reload error = %v, want missing injection", err)
	}
	value, ok := app.Resource("consumer")
	if !ok || value.(*runtimeExternalConsumer).pool != pool {
		t.Fatal("old generation stopped serving after failed reload")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times after failed reload", pool.closes)
	}
}

func TestRuntimeExternalResourceDynamicAgentDependency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := &runtimeExternalPool{}
	doc := runtimeExternalDoc(t, true)
	app := buildRuntimeExternal(t, doc, pool)

	if _, err := app.RegisterAgent(ctx, "dyn", agent.Definition{
		Card: agent.AgentCard{Name: "Dyn"},
		Engine: agent.EngineRef{
			Kind: "test.ExternalEngine",
			Impl: "runtime",
			Deps: resource.Deps{"db": "db"},
		},
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if result := runTurn(t, app.Sessions(), "dyn", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("dynamic turn status = %q", result.Status)
	}

	// Dynamic agents are re-bound against the new Result on Reload and
	// must still resolve the same external dependency.
	if _, err := app.Reload(ctx, doc); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if result := runTurn(t, app.Sessions(), "dyn", "conv2"); result.Status != agent.StatusCompleted {
		t.Fatalf("dynamic turn after reload status = %q", result.Status)
	}

	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times", pool.closes)
	}
}

func TestRuntimeExternalResourceDeclarationAloneSupportsDynamicAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := &runtimeExternalPool{}
	app := buildRuntimeExternal(t, runtimeExternalBareDoc(t), pool)

	if _, err := app.RegisterAgent(ctx, "dyn", agent.Definition{
		Card: agent.AgentCard{Name: "Dyn"},
		Engine: agent.EngineRef{
			Kind: "test.ExternalEngine",
			Impl: "runtime",
			Deps: resource.Deps{"db": "db"},
		},
	}); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if result := runTurn(t, app.Sessions(), "dyn", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("dynamic turn status = %q", result.Status)
	}
	if _, ok := app.Resource("consumer"); ok {
		t.Fatal("bare external document exposed a managed consumer")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times", pool.closes)
	}
}

func TestRuntimeExternalResourceBuilderValidation(t *testing.T) {
	t.Run("invalid resource", func(t *testing.T) {
		var typedNil *runtimeExternalPool
		tests := []ExternalResource{
			{ExternalDependency: ExternalDependency{Contract: "db.Pool"}, Value: &runtimeExternalPool{}},
			{ExternalDependency: ExternalDependency{Name: "db/item", Contract: "db.Pool"}, Value: &runtimeExternalPool{}},
			{ExternalDependency: ExternalDependency{Name: "db"}, Value: &runtimeExternalPool{}},
			{ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"}, Value: nil},
			{ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"}, Value: typedNil},
		}
		for _, ext := range tests {
			if err := ext.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate(%#v) error = %v, want validation", ext, err)
			}
		}
	})

	t.Run("plural and used builder", func(t *testing.T) {
		builder := NewBuilder(newRuntimeExternalRegistry(t))
		if err := builder.WithExternalResources([]ExternalResource{{
			ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"},
			Value:              &runtimeExternalPool{},
		}}); err != nil {
			t.Fatalf("WithExternalResources: %v", err)
		}
		app, err := builder.Build(context.Background(), runtimeExternalDoc(t, true))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		defer func() { _ = app.Close() }()

		if err := builder.WithExternalResource(ExternalResource{
			ExternalDependency: ExternalDependency{Name: "other", Contract: "other.Client"},
			Value:              &runtimeExternalPool{},
		}); !errors.Is(err, ErrBuilderUsed) {
			t.Fatalf("WithExternalResource after Build error = %v, want ErrBuilderUsed", err)
		}
	})

	t.Run("duplicate injection", func(t *testing.T) {
		builder := NewBuilder(newRuntimeExternalRegistry(t))
		for range 2 {
			if err := builder.WithExternalResource(ExternalResource{
				ExternalDependency: ExternalDependency{Name: "db", Contract: "db.Pool"},
				Value:              &runtimeExternalPool{},
			}); err != nil {
				t.Fatalf("WithExternalResource: %v", err)
			}
		}
		_, err := builder.Build(context.Background(), runtimeExternalDoc(t, true))
		if err == nil || !strings.Contains(err.Error(), "duplicate injected resource") {
			t.Fatalf("Build error = %v, want duplicate injection", err)
		}
	})

	t.Run("contract mismatch", func(t *testing.T) {
		builder := NewBuilder(newRuntimeExternalRegistry(t))
		if err := builder.WithExternalResource(ExternalResource{
			ExternalDependency: ExternalDependency{Name: "db", Contract: "cache.Client"},
			Value:              &runtimeExternalPool{},
		}); err != nil {
			t.Fatalf("WithExternalResource: %v", err)
		}
		_, err := builder.Build(context.Background(), runtimeExternalDoc(t, true))
		if err == nil || !strings.Contains(err.Error(), "does not match declared contract") {
			t.Fatalf("Build error = %v, want contract mismatch", err)
		}
	})
}
