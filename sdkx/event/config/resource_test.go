package config

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

type countingObserver struct {
	published atomic.Int64
}

func (o *countingObserver) OnPublish(event.Envelope)                     { o.published.Add(1) }
func (*countingObserver) OnDeliver(event.SubscriptionID, event.Envelope) {}
func (*countingObserver) OnDrop(event.SubscriptionID, event.Envelope, event.DropReason) {
}

func TestDeployFactorySpecs(t *testing.T) {
	want := deploy.ResourceSpec{Kind: ResourceKind, Impl: "memory"}
	if got := NewMemoryDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestMemoryDeployFactoryBuildsDefaultConfiguredAndInjectedBus(t *testing.T) {
	observer := &countingObserver{}
	tests := []struct {
		name          string
		factory       deploy.ResourceFactory
		settings      string
		wantCacheSize int64
	}{
		{
			name:          "default",
			factory:       NewMemoryDeployFactory(),
			wantCacheSize: 1024,
		},
		{
			name:          "configured",
			factory:       NewMemoryDeployFactory(event.WithObserver(observer)),
			settings:      "route_cache_size: 0\n",
			wantCacheSize: 0,
		},
		{
			name:          "negative keeps core default",
			factory:       NewMemoryDeployFactory(),
			settings:      "route_cache_size: -1\n",
			wantCacheSize: 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := tt.factory.New(context.Background(), deploy.ResourceInput{
				Settings: eventSettingsNode(t, tt.settings),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			bus, ok := value.(*event.MemoryBus)
			if !ok {
				t.Fatalf("New returned %T, want *event.MemoryBus", value)
			}
			cacheSize := reflect.ValueOf(bus).Elem().FieldByName("routeCacheCap").Int()
			if cacheSize != tt.wantCacheSize {
				t.Fatalf("route cache size = %d, want %d", cacheSize, tt.wantCacheSize)
			}
			if err := bus.Publish(context.Background(), event.Envelope{}); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			_ = bus.Close()
		})
	}
	if got := observer.published.Load(); got != 1 {
		t.Fatalf("observer publish calls = %d, want 1", got)
	}
}

func TestDeployFactoriesRejectUnknownSettings(t *testing.T) {
	factory := NewMemoryDeployFactory()
	if _, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: eventSettingsNode(t, "unknown: true\n"),
	}); err == nil {
		t.Fatal("memory factory accepted unknown settings")
	}
}

func TestEventBusBuildAndResourceAs(t *testing.T) {
	builder := deploy.NewBuilder(agent.NewRegistry())
	builder.MustRegisterResource(NewMemoryDeployFactory())

	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
agents: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := deploy.ResourceAs[event.Bus](result, "events")
	if err != nil {
		t.Fatalf("ResourceAs[event.Bus]: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := bus.Publish(context.Background(), event.Envelope{}); !errors.Is(err, event.ErrBusClosed) {
		t.Fatalf("Publish after Result.Close = %v, want ErrBusClosed", err)
	}
}

func eventSettingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	if input == "" {
		return nil
	}
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
