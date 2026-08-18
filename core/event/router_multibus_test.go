package event_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

func publish(t *testing.T, bus event.Bus, subject string) {
	t.Helper()
	if err := bus.Publish(context.Background(), event.Envelope{
		Subject: event.Subject(subject),
	}); err != nil {
		t.Fatalf("Publish(%s): %v", subject, err)
	}
}

func TestRouterAddBusDeliversToExistingAttachments(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	router := event.NewRouter(bus1)
	t.Cleanup(func() { _ = router.Close() })

	sink := &captureSink{}
	stop, err := router.Attach(context.Background(),
		event.Pattern("test.multi.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(stop)

	bus2 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus2.Close() })
	if err := router.AddBus(bus2); err != nil {
		t.Fatalf("AddBus: %v", err)
	}
	publish(t, bus2, "test.multi.two")
	waitFor(t, func() bool { return sink.count() == 1 })
}

func TestRouterRemoveBusStopsDeliveryFromThatBus(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	bus2 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus2.Close() })
	router := event.NewRouter(bus1)
	t.Cleanup(func() { _ = router.Close() })
	if err := router.AddBus(bus2); err != nil {
		t.Fatalf("AddBus: %v", err)
	}

	sink := &captureSink{}
	stop, err := router.Attach(context.Background(),
		event.Pattern("test.multi.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(stop)

	publish(t, bus1, "test.multi.one")
	publish(t, bus2, "test.multi.two")
	waitFor(t, func() bool { return sink.count() == 2 })

	if err := router.RemoveBus(bus2); err != nil {
		t.Fatalf("RemoveBus: %v", err)
	}
	publish(t, bus1, "test.multi.three")
	waitFor(t, func() bool { return sink.count() == 3 })
	publish(t, bus2, "test.multi.four")
	time.Sleep(50 * time.Millisecond)
	if sink.count() != 3 {
		t.Fatalf("deliveries after RemoveBus = %d, want 3", sink.count())
	}
}

func TestRouterAttachSubscribesEveryBus(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	bus2 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus2.Close() })
	router := event.NewRouter(bus1)
	t.Cleanup(func() { _ = router.Close() })
	if err := router.AddBus(bus2); err != nil {
		t.Fatalf("AddBus: %v", err)
	}

	sink := &captureSink{}
	stop, err := router.Attach(context.Background(),
		event.Pattern("test.multi.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(stop)

	publish(t, bus1, "test.multi.one")
	publish(t, bus2, "test.multi.two")
	waitFor(t, func() bool { return sink.count() == 2 })
}

func TestRouterAddBusErrors(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	router := event.NewRouter(bus1)

	if err := router.AddBus(bus1); !errdefs.IsConflict(err) {
		t.Fatalf("AddBus(duplicate) error = %v, want conflict", err)
	}
	if err := router.AddBus(nil); !errdefs.IsValidation(err) {
		t.Fatalf("AddBus(nil) error = %v, want validation", err)
	}
	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := router.AddBus(event.NewMemoryBus()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("AddBus after Close error = %v, want not available", err)
	}
}

func TestRouterRemoveBusUnknownIsNoop(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	router := event.NewRouter(bus1)
	t.Cleanup(func() { _ = router.Close() })
	if err := router.RemoveBus(event.NewMemoryBus()); err != nil {
		t.Fatalf("RemoveBus(unknown) error = %v, want nil", err)
	}
}

// TestRouterConcurrentAddBusClose exercises the WaitGroup registration
// ordering under -race: AddBus must register its goroutines before
// releasing the lock, so a concurrent Close can never observe a zero
// counter while Add is still running.
func TestRouterConcurrentAddBusClose(t *testing.T) {
	bus1 := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus1.Close() })
	router := event.NewRouter(bus1)

	sink := &captureSink{}
	stop, err := router.Attach(context.Background(),
		event.Pattern("test.multi.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			b := event.NewMemoryBus()
			if err := router.AddBus(b); err != nil {
				return // router closed
			}
			time.Sleep(time.Microsecond)
		}
	}()
	time.Sleep(2 * time.Millisecond)
	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
}
