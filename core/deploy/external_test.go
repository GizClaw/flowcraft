package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

type externalPool struct {
	closes int
	wires  int
	binds  int
}

func (p *externalPool) Close() error {
	p.closes++
	return nil
}

func (p *externalPool) Wire(context.Context) error {
	p.wires++
	return nil
}

func (p *externalPool) BindDeployment(View) error {
	p.binds++
	return nil
}

type externalConsumer struct {
	pool   *externalPool
	closed bool
}

func (c *externalConsumer) Close() error {
	c.closed = true
	return nil
}

// depPool is the typed dependency every double in this file reads: the
// caller-injected pool. The requirement is the only place the contract is
// written, and the plan checks the injected value's Go type against it.
var depPool = resource.Require[*externalPool]("db")

type externalConsumerFactory struct{}

func (externalConsumerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalConsumer",
		Impl: "local",
		Deps: []resource.Requirement{depPool.Spec()},
	}
}

func (externalConsumerFactory) New(_ context.Context, in resource.Input) (*externalConsumer, error) {
	pool, err := depPool.Get(in)
	if err != nil {
		return nil, err
	}
	return &externalConsumer{pool: pool}, nil
}

// externalFailingFactory builds nothing: it exists so a deployment can fail
// after another resource is already built, which is what exercises rollback.
type externalFailingFactory struct{}

func (externalFailingFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalFail",
		Impl: "local",
		Deps: []resource.Requirement{resource.Require[*externalConsumer]("consumer").Spec()},
	}
}

func (externalFailingFactory) New(context.Context, resource.Input) (any, error) {
	return nil, errors.New("boom")
}

type externalAgentEngineFactory struct{}

func (externalAgentEngineFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "engine.test",
		Impl: "external",
		Deps: []resource.Requirement{depPool.Spec()},
	}
}

func (externalAgentEngineFactory) New(_ context.Context, in resource.Input) (agent.Engine, error) {
	if _, err := depPool.Get(in); err != nil {
		return nil, err
	}
	return agent.EngineFunc(func(
		context.Context, agent.Run, agent.Host, *agent.Board,
	) (*agent.Board, error) {
		return agent.NewBoard(), nil
	}), nil
}

type externalObserverHook struct {
	agent.BaseObserver
	pool *externalPool
}

type externalObserverHookFactory struct{}

func (externalObserverHookFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "hook.observe",
		Impl: "external",
		Deps: []resource.Requirement{depPool.Spec()},
	}
}

func (externalObserverHookFactory) New(_ context.Context, in resource.Input) (agent.Observer, error) {
	pool, err := depPool.Get(in)
	if err != nil {
		return nil, err
	}
	return &externalObserverHook{pool: pool}, nil
}

func externalDoc() Document {
	return Document{
		Version: "v1",
		Resources: resource.Resources{
			"consumer": {
				Kind: "test.ExternalConsumer",
				Impl: "local",
				Deps: resource.Deps{"db": "db"},
			},
		},
	}
}

func TestExternalResourceResolvesAsDependencyAndStaysUnmanaged(t *testing.T) {
	pool := &externalPool{}
	reg := resource.NewRegistry()
	resource.MustRegister(reg, externalConsumerFactory{})
	builder := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{NewExternal("db", "db.Pool", pool)}),
	)

	result, err := builder.Deploy(context.Background(), externalDoc())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	resultClosed := false
	defer func() {
		if !resultClosed {
			_ = result.Close()
		}
	}()

	if _, ok := result.Value("db"); ok {
		t.Fatal("external db leaked into managed Value view")
	}
	for _, name := range result.Names() {
		if name == "db" {
			t.Fatal("external db leaked into managed Names view")
		}
	}
	value, ok := result.Value("consumer")
	if !ok {
		t.Fatalf("consumer resource missing")
	}
	consumer := value.(*externalConsumer)
	if consumer.pool != pool {
		t.Fatal("consumer did not receive the injected external value")
	}
	if err := result.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	resultClosed = true
	if pool.closes != 0 || pool.wires != 0 || pool.binds != 0 {
		t.Fatalf(
			"external was managed: closes=%d wires=%d binds=%d",
			pool.closes, pool.wires, pool.binds)
	}
	if !consumer.closed {
		t.Fatal("managed consumer was not closed")
	}
}

// The contract is the Go type now, so a mismatch is an injected value that
// does not satisfy the consumer's requirement — reported by Plan, before the
// consumer factory runs.
func TestExternalResourceContractMismatch(t *testing.T) {
	reg := resource.NewRegistry()
	resource.MustRegister(reg, externalConsumerFactory{})
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{NewExternal("db", "db.Other", "not a pool")}),
	).Build(context.Background(), externalDoc())
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("Build error = %v, want a Go type mismatch", err)
	}
}

func TestExternalResourceRejectsItemRef(t *testing.T) {
	doc := externalDoc()
	consumer := doc.Resources["consumer"]
	consumer.Deps = resource.Deps{"db": "db/item"}
	doc.Resources["consumer"] = consumer
	reg := resource.NewRegistry()
	resource.MustRegister(reg, externalConsumerFactory{})
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{NewExternal("db", "db.Pool", &externalPool{})}),
	).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "does not support item references") {
		t.Fatalf("Build error = %v, want item-ref rejection", err)
	}
}

func TestExternalResourceNotClosedOnRollback(t *testing.T) {
	pool := &externalPool{}
	reg := resource.NewRegistry()
	resource.MustRegister(reg, externalConsumerFactory{})
	resource.MustRegister(reg, externalFailingFactory{})
	doc := externalDoc()
	doc.Resources["z-fail"] = resource.Resource{
		Kind: "test.ExternalFail",
		Impl: "local",
		Deps: resource.Deps{"consumer": "consumer"},
	}
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{NewExternal("db", "db.Pool", pool)}),
	).Build(context.Background(), doc)
	if err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}
	if pool.closes != 0 {
		t.Fatalf("external closed during rollback: %d times", pool.closes)
	}
}

func TestExternalResourceResolvesForAgentEngineAndHook(t *testing.T) {
	pool := &externalPool{}
	reg := resource.NewRegistry()
	resource.MustRegister(reg, externalAgentEngineFactory{})
	resource.MustRegister(reg, externalObserverHookFactory{})
	doc := Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"bot": {
				Card: agent.AgentCard{Name: "Bot"},
				Engine: agent.EngineRef{
					Kind: "engine.test",
					Impl: "external",
					Deps: resource.Deps{"db": "db"},
				},
				Observe: []agent.Hook{{
					Type: "external",
					Deps: resource.Deps{"db": "db"},
				}},
			},
		},
	}
	result, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{NewExternal("db", "db.Pool", pool)}),
	).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	instance, ok := result.Agent("bot")
	if !ok {
		t.Fatal("bot agent missing")
	}
	if len(instance.Observe) != 1 {
		t.Fatalf("observe hooks = %d, want 1", len(instance.Observe))
	}
	hook, ok := instance.Observe[0].(*externalObserverHook)
	if !ok {
		t.Fatalf("hook type = %T, want *externalObserverHook", instance.Observe[0])
	}
	if hook.pool != pool {
		t.Fatal("hook did not receive the injected external pool")
	}
	if err := result.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pool.closes != 0 {
		t.Fatalf("external pool was closed %d times", pool.closes)
	}
}

func TestExternalResourceValidation(t *testing.T) {
	var typedNil *externalPool
	tests := []struct {
		name string
		ext  ExternalResource
	}{
		{name: "missing name", ext: ExternalResource{
			External: External{Contract: "db.Pool"}, Value: &externalPool{},
		}},
		{name: "name with slash", ext: ExternalResource{
			External: External{Name: "db/item", Contract: "db.Pool"}, Value: &externalPool{},
		}},
		{name: "missing contract", ext: ExternalResource{
			External: External{Name: "db"}, Value: &externalPool{},
		}},
		{name: "nil value", ext: ExternalResource{
			External: External{Name: "db", Contract: "db.Pool"}, Value: nil,
		}},
		{name: "typed nil value", ext: ExternalResource{
			External: External{Name: "db", Contract: "db.Pool"}, Value: typedNil,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.ext.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate error = %v, want validation", err)
			}
			if _, err := normalizeExternalResources([]ExternalResource{test.ext}); !errdefs.IsValidation(err) {
				t.Fatalf("normalize error = %v, want validation", err)
			}
		})
	}

	t.Run("duplicate", func(t *testing.T) {
		_, err := normalizeExternalResources([]ExternalResource{
			NewExternal("db", "db.Pool", &externalPool{}),
			NewExternal("db", "db.Pool", &externalPool{}),
		})
		if !errdefs.IsConflict(err) {
			t.Fatalf("normalize duplicate error = %v, want conflict", err)
		}
	})
}
