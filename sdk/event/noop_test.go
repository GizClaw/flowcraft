package event

import (
	"context"
	"testing"
	"time"
)

// TestNoopBus_AllMethods exercises every method of NoopBus and the
// noopSubscription it returns. NoopBus is the documented "drop everything"
// fallback for callers that want bus-shaped APIs without a real bus, so
// each method must succeed and produce the canonical zero behaviour.
func TestNoopBus_AllMethods(t *testing.T) {
	bus := NewNoopBus()

	if err := bus.Publish(context.Background(), Envelope{Subject: "x"}); err != nil {
		t.Errorf("NoopBus.Publish: %v", err)
	}

	sub, err := bus.Subscribe(context.Background(), Pattern(">"))
	if err != nil {
		t.Fatalf("NoopBus.Subscribe: %v", err)
	}

	if sub.ID() != "noop" {
		t.Errorf("noopSubscription.ID() = %q, want %q", sub.ID(), "noop")
	}

	// noopSubscription.C() returns an already-closed channel, so the
	// receive must complete immediately with the zero value and !ok.
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("noopSubscription channel should be closed (ok=false)")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("noopSubscription channel should be already-closed")
	}

	if err := sub.Close(); err != nil {
		t.Errorf("noopSubscription.Close: %v", err)
	}
	// Close is idempotent.
	if err := sub.Close(); err != nil {
		t.Errorf("noopSubscription.Close (second call): %v", err)
	}

	if err := bus.Close(); err != nil {
		t.Errorf("NoopBus.Close: %v", err)
	}
}

// TestNoopBus_AsBusInterface guarantees NoopBus satisfies the Bus
// interface — guards against accidentally narrowing the surface.
func TestNoopBus_AsBusInterface(t *testing.T) {
	var _ Bus = NoopBus{}
	var _ Bus = NewNoopBus()
	var _ Subscription = noopSubscription{}
}
