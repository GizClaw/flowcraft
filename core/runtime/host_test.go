package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

func TestBaseHostCheckpointPersists(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	store := &recordingCheckpointStore{}
	factory, err := newBaseHostFactory(bus, store)
	if err != nil {
		t.Fatal(err)
	}
	host := mustBaseHost(t, factory)

	cp := agent.Checkpoint{
		ExecID: "run-1",
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}
	if err := host.Checkpoint(context.Background(), cp); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	store.mu.Lock()
	saved := store.saved
	store.mu.Unlock()
	if saved == nil || saved.ExecID != "run-1" || len(saved.Steps) != 1 {
		t.Fatalf("store.saved = %+v, want run-1 checkpoint", saved)
	}
}

func TestBaseHostCheckpointDropsWithoutStore(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	factory, err := newBaseHostFactory(bus)
	if err != nil {
		t.Fatal(err)
	}
	host := mustBaseHost(t, factory)
	if err := host.Checkpoint(context.Background(), agent.Checkpoint{
		ExecID: "run-1",
		Board:  agent.NewBoard().Snapshot(),
	}); err != nil {
		t.Fatalf("Checkpoint without store = %v, want nil", err)
	}
}

func TestBaseHostPublishesAndExposesBorrowedBus(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	factory, err := newBaseHostFactory(bus)
	if err != nil {
		t.Fatal(err)
	}
	first := mustBaseHost(t, factory)
	second := mustBaseHost(t, factory)
	if reflect.ValueOf(first).Pointer() == reflect.ValueOf(second).Pointer() {
		t.Fatal("base factory reused a Host across turns")
	}
	provider, ok := first.(agent.EventBusProvider)
	if !ok || provider.EventBus() != bus {
		t.Fatal("base Host did not expose the borrowed event bus")
	}

	sub, err := bus.Subscribe(context.Background(), "runtime.test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	envelope, err := event.NewEnvelope(context.Background(), "runtime.test", map[string]string{"ok": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Publish(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.C():
	case <-time.After(time.Second):
		t.Fatal("published envelope did not reach explicit bus")
	}
}

func TestBaseHostRejectsInvalidRequest(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	factory, err := newBaseHostFactory(bus)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.NewHost(context.Background(), session.HostRequest{}); err == nil {
		t.Fatal("NewHost with empty request unexpectedly succeeded")
	}
}
