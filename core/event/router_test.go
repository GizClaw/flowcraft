package event_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

type captureSink struct {
	mu   sync.Mutex
	got  []event.Envelope
	fail bool
}

func (s *captureSink) OnEnvelope(_ context.Context, env event.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("sink exploded")
	}
	s.got = append(s.got, env)
	return nil
}

func (s *captureSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func TestRouterFansOutToAllAttachments(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := event.NewRouter(bus)
	t.Cleanup(func() { _ = router.Close() })

	var sinks [2]*captureSink
	for i := range sinks {
		sinks[i] = &captureSink{}
		stop, err := router.Attach(context.Background(),
			event.Pattern("test.router.*"), sinks[i])
		if err != nil {
			t.Fatalf("Attach %d: %v", i, err)
		}
		t.Cleanup(stop)
	}

	if err := bus.Publish(context.Background(), event.Envelope{
		Subject: "test.router.one",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, func() bool { return sinks[0].count() == 1 && sinks[1].count() == 1 })
}

func TestRouterDetachStopsDelivery(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := event.NewRouter(bus)
	t.Cleanup(func() { _ = router.Close() })

	sink := &captureSink{}
	stop, err := router.Attach(context.Background(), event.Pattern("test.detach.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := bus.Publish(context.Background(), event.Envelope{Subject: "test.detach.a"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.count() == 1 })

	stop()
	// Give the router a beat to tear down before publishing again.
	time.Sleep(10 * time.Millisecond)
	if err := bus.Publish(context.Background(), event.Envelope{Subject: "test.detach.b"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := sink.count(); got != 1 {
		t.Fatalf("deliveries after detach = %d, want 1", got)
	}
}

func TestRouterSinkErrorDetachesAndReports(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := event.NewRouter(bus)
	t.Cleanup(func() { _ = router.Close() })

	var detached atomicBool
	sink := &captureSink{fail: true}
	stop, err := router.Attach(context.Background(), event.Pattern("test.fail.*"), sink,
		event.WithOnDetach(func(error) { detached.store(true) }))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(stop)

	if err := bus.Publish(context.Background(), event.Envelope{Subject: "test.fail.a"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, detached.load)
}

func TestRouterCloseStopsEverything(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := event.NewRouter(bus)
	sink := &captureSink{}
	if _, err := router.Attach(context.Background(), event.Pattern("test.close.*"), sink); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := router.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := router.Attach(context.Background(), event.Pattern("test.after.*"), sink); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Attach after Close = %v, want NotAvailable", err)
	}
}

func TestRouterDefaultBackpressureBlocks(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := event.NewRouter(bus, event.WithDefaultAttachBackpressure(event.Block))
	t.Cleanup(func() { _ = router.Close() })

	release := make(chan struct{})
	sink := &gatedSink{release: release}
	if _, err := router.Attach(context.Background(),
		event.Pattern(">"), sink, event.WithAttachBufferSize(1)); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	for _, subject := range []string{"block.one", "block.two"} {
		if err := bus.Publish(context.Background(), event.Envelope{Subject: event.Subject(subject)}); err != nil {
			t.Fatalf("Publish %s: %v", subject, err)
		}
	}
	published := make(chan struct{})
	go func() {
		_ = bus.Publish(context.Background(), event.Envelope{Subject: "block.three"})
		close(published)
	}()
	select {
	case <-published:
		t.Fatal("Publish should block when the subscription buffer is full")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("Publish did not unblock after the sink consumed")
	}
	waitFor(t, func() bool { return sink.count() == 3 })
}

type gatedSink struct {
	release chan struct{}
	mu      sync.Mutex
	got     []event.Envelope
}

func (s *gatedSink) OnEnvelope(_ context.Context, env event.Envelope) error {
	<-s.release
	s.mu.Lock()
	s.got = append(s.got, env)
	s.mu.Unlock()
	return nil
}

func (s *gatedSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) store(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) load() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}
