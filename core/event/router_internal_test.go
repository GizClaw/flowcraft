package event

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRouterContextCancelDetaches(t *testing.T) {
	bus := NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := NewRouter(bus)
	t.Cleanup(func() { _ = router.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	sink := &countingSink{}
	if _, err := router.Attach(ctx, Pattern("test.ctx.*"), sink); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := bus.Publish(context.Background(), Envelope{Subject: "test.ctx.a"}); err != nil {
		t.Fatal(err)
	}
	waitRouterFor(t, func() bool { return sink.count() == 1 })

	cancel()
	waitRouterFor(t, func() bool { return router.attachmentCount() == 0 })
	if err := bus.Publish(context.Background(), Envelope{Subject: "test.ctx.b"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := sink.count(); got != 1 {
		t.Fatalf("deliveries after context cancel = %d, want 1", got)
	}
}

func (r *Router) attachmentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attachments)
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (s *countingSink) OnEnvelope(context.Context, Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil
}

func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func waitRouterFor(t *testing.T, cond func() bool) {
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
