package deploy_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// The tests in this file pin what the typed protocol buys over the old
// string-contract one: a deployment is judged before anything is constructed,
// and a wiring mistake names the document resource and dep that caused it.

type planPool struct{}

type planCache struct{}

// planConsumer requires a *planPool, so a *planCache edge is a type error.
type planConsumer struct{ constructed int }

func (planConsumerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.PlanConsumer",
		Impl: "local",
		Deps: []resource.Requirement{resource.Require[*planPool]("db").Spec()},
	}
}

type planConsumerFactory struct{ built *int }

func (f planConsumerFactory) New(_ context.Context, in resource.Input) (*planConsumer, error) {
	*f.built++
	if _, err := resource.Require[*planPool]("db").Get(in); err != nil {
		return nil, err
	}
	return &planConsumer{constructed: *f.built}, nil
}

type planPoolFactory struct{ value *planPool }

func (planPoolFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "test.PlanPool", Impl: "local"}
}

func (f planPoolFactory) New(context.Context, resource.Input) (*planPool, error) {
	return f.value, nil
}

type planCacheFactory struct{}

func (planCacheFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "test.PlanCache", Impl: "local"}
}

func (planCacheFactory) New(context.Context, resource.Input) (*planCache, error) {
	return &planCache{}, nil
}

func planDoc(consumerDeps resource.Deps) deploy.Document {
	return deploy.Document{
		Version:   "v1",
		Resources: resource.Resources{"db": {Kind: "test.PlanCache", Impl: "local"}, "user": {Kind: "test.PlanConsumer", Impl: "local", Deps: consumerDeps}},
	}
}

func TestPlanRejectsDepTypeMismatchBeforeConstruction(t *testing.T) {
	built := 0
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planConsumerFactory{built: &built})
	resource.MustRegister(reg, planCacheFactory{})

	_, err := deploy.NewBuilder(reg).Build(context.Background(),
		planDoc(resource.Deps{"db": "db"}))
	if !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	if !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("Build error = %v, want a Go type mismatch", err)
	}
	if !strings.Contains(err.Error(), `resources["user"]`) ||
		!strings.Contains(err.Error(), `dep "db"`) {
		t.Fatalf("Build error = %v, want the resource and dep named", err)
	}
	if built != 0 {
		t.Fatalf("consumer factory ran %d times, want 0: the plan must reject before construction", built)
	}
}

func TestPlanReportsEveryBadEdgeAtOnce(t *testing.T) {
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planConsumerFactory{built: new(int)})
	resource.MustRegister(reg, planCacheFactory{})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"db": {Kind: "test.PlanCache", Impl: "local"},
			"a":  {Kind: "test.PlanConsumer", Impl: "local", Deps: resource.Deps{"db": "db"}},
			"b":  {Kind: "test.PlanConsumer", Impl: "local", Deps: resource.Deps{"db": "db"}},
			"c":  {Kind: "test.PlanConsumer", Impl: "local", Deps: resource.Deps{"bogus": "db"}},
		},
	}
	_, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if err == nil {
		t.Fatal("Build unexpectedly succeeded")
	}
	msg := err.Error()
	for _, want := range []string{`resources["a"]`, `resources["b"]`, `resources["c"]`, `"bogus"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Build error must report %q, got:\n%s", want, msg)
		}
	}
}

func TestBuildRejectsTypedNilDependency(t *testing.T) {
	var typedNil *planPool
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planConsumerFactory{built: new(int)})
	// The producer declares *planPool, so the plan accepts the edge; the value
	// it returns is a typed nil, which the typed read must reject rather than
	// hand to the consumer.
	resource.MustRegister(reg, planPoolFactory{value: typedNil})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"db":   {Kind: "test.PlanPool", Impl: "local"},
			"user": {Kind: "test.PlanConsumer", Impl: "local", Deps: resource.Deps{"db": "db"}},
		},
	}
	_, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if err == nil {
		t.Fatal("Build accepted a typed-nil dependency")
	}
	if !strings.Contains(err.Error(), "typed nil") {
		t.Fatalf("Build error = %v, want a typed-nil diagnostic", err)
	}
}

type planItem struct{ ID string }

type planContainer struct{ items map[string]*planItem }

func (c *planContainer) ResolveItem(item string) (any, bool) {
	v, ok := c.items[item]
	return v, ok
}

type planContainerFactory struct{}

func (planContainerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind:  "test.PlanContainer",
		Impl:  "local",
		Items: reflect.TypeFor[*planItem](),
	}
}

func (planContainerFactory) New(context.Context, resource.Input) (*planContainer, error) {
	return &planContainer{items: map[string]*planItem{"one": {ID: "one"}}}, nil
}

type planItemConsumer struct{ got *planItem }

func (itemConsumerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "test.PlanItemConsumer",
		Impl: "local",
		Deps: []resource.Requirement{resource.Require[*planItem]("item").Spec()},
	}
}

type itemConsumerFactory struct{ built *int }

func (f itemConsumerFactory) New(_ context.Context, in resource.Input) (*planItemConsumer, error) {
	if f.built != nil {
		*f.built++
	}
	item, err := resource.Require[*planItem]("item").Get(in)
	if err != nil {
		return nil, err
	}
	return &planItemConsumer{got: item}, nil
}

func TestPlanChecksItemRefType(t *testing.T) {
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planContainerFactory{})
	resource.MustRegister(reg, itemConsumerFactory{})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"box":  {Kind: "test.PlanContainer", Impl: "local"},
			"user": {Kind: "test.PlanItemConsumer", Impl: "local", Deps: resource.Deps{"item": "box/one"}},
		},
	}
	result, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	defer func() { _ = result.Close() }()

	consumer, err := deploy.Resolve[*planItemConsumer](result, "user")
	if err != nil {
		t.Fatalf("resolve consumer: %v", err)
	}
	if consumer.got == nil || consumer.got.ID != "one" {
		t.Fatalf("consumer item = %+v, want the box/one item", consumer.got)
	}
}

func TestPlanRejectsItemRefFromNonContainer(t *testing.T) {
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planCacheFactory{})
	resource.MustRegister(reg, itemConsumerFactory{})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"box":  {Kind: "test.PlanCache", Impl: "local"},
			"user": {Kind: "test.PlanItemConsumer", Impl: "local", Deps: resource.Deps{"item": "box/one"}},
		},
	}
	_, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "does not expose items") {
		t.Fatalf("Build error = %v, want an item-ref rejection", err)
	}
}

// planOtherItem never satisfies a *planItem requirement.
type planOtherItem struct{ ID string }

// planTypedContainer exposes *planOtherItem items, so a consumer requiring
// *planItem is a type error the plan can see without building anything.
type planTypedContainer struct{}

func (planTypedContainerFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind:  "test.PlanOtherContainer",
		Impl:  "local",
		Items: reflect.TypeFor[*planOtherItem](),
	}
}

type planTypedContainerFactory struct{ built *int }

func (f planTypedContainerFactory) New(context.Context, resource.Input) (*planTypedContainer, error) {
	*f.built++
	return &planTypedContainer{}, nil
}

func TestPlanRejectsItemRefTypeMismatchBeforeConstruction(t *testing.T) {
	built := 0
	consumerBuilt := 0
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planTypedContainerFactory{built: &built})
	resource.MustRegister(reg, itemConsumerFactory{built: &consumerBuilt})

	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"box":  {Kind: "test.PlanOtherContainer", Impl: "local"},
			"user": {Kind: "test.PlanItemConsumer", Impl: "local", Deps: resource.Deps{"item": "box/one"}},
		},
	}
	_, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("Build error = %v, want an item-type mismatch", err)
	}
	if !strings.Contains(err.Error(), `resources["user"]`) {
		t.Fatalf("Build error = %v, want the consumer named", err)
	}
	if built != 0 || consumerBuilt != 0 {
		t.Fatalf("factories ran (container=%d consumer=%d), want 0", built, consumerBuilt)
	}
}

// wrongEngineFactory produces something that is not an agent.Engine, so an
// agent that names it must be rejected by the plan, not by a panic later.
type wrongEngineFactory struct{}

func (wrongEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "engine.test", Impl: "wrong"}
}

func (wrongEngineFactory) New(context.Context, resource.Input) (*planConsumer, error) {
	return &planConsumer{}, nil
}

// wrongHook is not an agent.Observer.
type wrongHook struct{}

func (wrongHookFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "hook.observe", Impl: "wrong"}
}

type wrongHookFactory struct{}

func (wrongHookFactory) New(context.Context, resource.Input) (*wrongHook, error) {
	return &wrongHook{}, nil
}

func TestPlanRejectsWrongEngineAndHookSocketTypes(t *testing.T) {
	reg := resource.NewRegistry()
	resource.MustRegister(reg, wrongEngineFactory{})
	resource.MustRegister(reg, wrongHookFactory{})

	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"bot": {
				Card:    agent.AgentCard{Name: "Bot"},
				Engine:  agent.EngineRef{Kind: "engine.test", Impl: "wrong"},
				Observe: []agent.Hook{{Type: "wrong"}},
			},
		},
	}
	_, err := deploy.NewBuilder(reg).Build(context.Background(), doc)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "want agent.Engine") {
		t.Fatalf("Build error = %v, want the engine socket named", msg)
	}
	if !strings.Contains(msg, "want agent.Observer") {
		t.Fatalf("Build error = %v, want the hook socket named", msg)
	}
}

// Plan is advertised as a preflight: calling it alone must validate without
// constructing, and the returned plan must then build.
func TestPlanAloneValidatesThenBuilds(t *testing.T) {
	built := 0
	reg := resource.NewRegistry()
	resource.MustRegister(reg, planConsumerFactory{built: &built})
	resource.MustRegister(reg, planPoolFactory{value: &planPool{}})

	builder := deploy.NewBuilder(reg)
	plan, err := builder.Plan(deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"db":   {Kind: "test.PlanPool", Impl: "local"},
			"user": {Kind: "test.PlanConsumer", Impl: "local", Deps: resource.Deps{"db": "db"}},
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if built != 0 {
		t.Fatalf("Plan constructed %d resources, want 0", built)
	}
	result, err := plan.Build(context.Background())
	if err != nil {
		t.Fatalf("Plan.Build: %v", err)
	}
	defer func() { _ = result.Close() }()
	if built != 1 {
		t.Fatalf("Plan.Build constructed %d consumers, want 1", built)
	}
}
