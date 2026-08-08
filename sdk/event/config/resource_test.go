package config

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/event"
)

type countingObserver struct {
	published atomic.Int64
}

func (o *countingObserver) OnPublish(event.Envelope)                     { o.published.Add(1) }
func (*countingObserver) OnDeliver(event.SubscriptionID, event.Envelope) {}
func (*countingObserver) OnDrop(event.SubscriptionID, event.Envelope, event.DropReason) {
}

func TestDeployFactorySpecs(t *testing.T) {
	want := sdkconfig.Spec{Kind: ResourceKind, Impl: "memory"}
	if got := NewMemoryDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestMemoryDeployFactoryBuildsDefaultConfiguredAndInjectedBus(t *testing.T) {
	observer := &countingObserver{}
	tests := []struct {
		name          string
		factory       sdkconfig.Factory
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
			settings:      `{"route_cache_size":0}`,
			wantCacheSize: 0,
		},
		{
			name:          "negative keeps core default",
			factory:       NewMemoryDeployFactory(),
			settings:      `{"route_cache_size":-1}`,
			wantCacheSize: 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := tt.factory.New(context.Background(), sdkconfig.Input{
				Settings: settingsJSON(t, tt.settings),
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
	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"unknown":true}`),
	}); err == nil {
		t.Fatal("memory factory accepted unknown settings")
	}
}

func settingsJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	if raw == "" {
		return nil
	}
	var out json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return out
}
