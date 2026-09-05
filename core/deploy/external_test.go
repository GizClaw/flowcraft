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

func (p *externalPool) BindDeployment(any) error {
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

type externalConsumerFactory struct{}

func (externalConsumerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalConsumer",
		Impl: "local",
		Deps: []resource.DepSpec{{
			Name: "db", Type: "db.Pool", Required: true,
		}},
	}
}

func (externalConsumerFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("db")
	if !ok {
		return nil, errdefs.Validationf("consumer: db dep missing")
	}
	pool, ok := value.(*externalPool)
	if !ok {
		return nil, errdefs.Validationf("consumer: db dep has wrong type")
	}
	return &externalConsumer{pool: pool}, nil
}

type externalFailingFactory struct {
	consumerType string
}

func (f externalFailingFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.ExternalFail",
		Impl: "local",
		Deps: []resource.DepSpec{{
			Name: "consumer", Type: f.consumerType, Required: true,
		}},
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
		Deps: []resource.DepSpec{{
			Name: "db", Type: "db.Pool", Required: true,
		}},
	}
}

func (externalAgentEngineFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("db")
	if !ok {
		return nil, errdefs.Validationf("engine: db dep missing")
	}
	if _, ok := value.(*externalPool); !ok {
		return nil, errdefs.Validationf("engine: db dep has wrong type")
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
		Deps: []resource.DepSpec{{
			Name: "db", Type: "db.Pool", Required: true,
		}},
	}
}

func (externalObserverHookFactory) New(_ context.Context, in resource.Input) (any, error) {
	value, ok := in.Dep("db")
	if !ok {
		return nil, errdefs.Validationf("hook: db dep missing")
	}
	pool, ok := value.(*externalPool)
	if !ok {
		return nil, errdefs.Validationf("hook: db dep has wrong type")
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
	reg.MustRegister(externalConsumerFactory{})
	builder := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{{
			External: External{Name: "db", Contract: "db.Pool"},
			Value:    pool,
		}}),
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

func TestExternalResourceContractMismatch(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(externalConsumerFactory{})
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{{
			External: External{Name: "db", Contract: "db.Other"},
			Value:    &externalPool{},
		}}),
	).Build(context.Background(), externalDoc())
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "does not match declared type") {
		t.Fatalf("Build error = %v, want contract mismatch", err)
	}
}

func TestExternalResourceRejectsItemRef(t *testing.T) {
	doc := externalDoc()
	consumer := doc.Resources["consumer"]
	consumer.Deps = resource.Deps{"db": "db/item"}
	doc.Resources["consumer"] = consumer
	reg := resource.NewRegistry()
	reg.MustRegister(externalConsumerFactory{})
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{{
			External: External{Name: "db", Contract: "db.Pool"},
			Value:    &externalPool{},
		}}),
	).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "does not support item references") {
		t.Fatalf("Build error = %v, want item-ref rejection", err)
	}
}

func TestExternalResourceNotClosedOnRollback(t *testing.T) {
	pool := &externalPool{}
	reg := resource.NewRegistry()
	reg.MustRegister(externalConsumerFactory{})
	reg.MustRegister(externalFailingFactory{consumerType: "test.ExternalConsumer"})
	doc := externalDoc()
	doc.Resources["z-fail"] = resource.Resource{
		Kind: "test.ExternalFail",
		Impl: "local",
		Deps: resource.Deps{"consumer": "consumer"},
	}
	_, err := NewBuilder(
		reg,
		WithExternalResources([]ExternalResource{{
			External: External{Name: "db", Contract: "db.Pool"},
			Value:    pool,
		}}),
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
	reg.MustRegister(externalAgentEngineFactory{})
	reg.MustRegister(externalObserverHookFactory{})
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
		WithExternalResources([]ExternalResource{{
			External: External{Name: "db", Contract: "db.Pool"},
			Value:    pool,
		}}),
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
			{External: External{Name: "db", Contract: "db.Pool"}, Value: &externalPool{}},
			{External: External{Name: "db", Contract: "db.Pool"}, Value: &externalPool{}},
		})
		if !errdefs.IsConflict(err) {
			t.Fatalf("normalize duplicate error = %v, want conflict", err)
		}
	})
}
