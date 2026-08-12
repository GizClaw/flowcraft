package event_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := event.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup("event.Bus", "memory")
	if !ok {
		t.Fatal("event.Bus/memory factory not registered")
	}
	value, err := factory.New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*event.MemoryBus); !ok {
		t.Fatalf("New returned %T, want *event.MemoryBus", value)
	}
}

func TestRegisterRejectsSettings(t *testing.T) {
	reg := resource.NewRegistry()
	if err := event.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("event.Bus", "memory")
	if _, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"bogus": 1}`),
	}); err == nil {
		t.Fatal("New unexpectedly accepted unknown settings")
	}
}

func TestFactoryAppliesRouteCacheSize(t *testing.T) {
	reg := resource.NewRegistry()
	if err := event.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("event.Bus", "memory")
	if _, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"route_cache_size": 0}`),
	}); err != nil {
		t.Fatalf("New with route_cache_size: %v", err)
	}
}

type recordingObserver struct {
	published int
}

func (o *recordingObserver) OnPublish(event.Envelope)                     { o.published++ }
func (*recordingObserver) OnDeliver(event.SubscriptionID, event.Envelope) {}
func (*recordingObserver) OnDrop(event.SubscriptionID, event.Envelope, event.DropReason) {
}

func TestFactoryInjectsObserver(t *testing.T) {
	obs := &recordingObserver{}
	factory := event.NewFactory(event.WithObserver(obs))
	value, err := factory.New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bus := value.(*event.MemoryBus)
	if err := bus.Publish(context.Background(), event.Envelope{Subject: "test.event"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if obs.published != 1 {
		t.Fatalf("observer published = %d, want 1", obs.published)
	}
}

func TestMemoryBusAttachObserver(t *testing.T) {
	bus := event.NewMemoryBus()
	obs := &recordingObserver{}
	if err := bus.AttachObserver(obs); err != nil {
		t.Fatalf("AttachObserver: %v", err)
	}
	if err := bus.Publish(context.Background(), event.Envelope{Subject: "test.event"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if obs.published != 1 {
		t.Fatalf("observer published = %d, want 1", obs.published)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bus.AttachObserver(obs); !errors.Is(err, event.ErrBusClosed) {
		t.Fatalf("AttachObserver after close = %v, want ErrBusClosed", err)
	}
}
