package event

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryBus_PublishSubscribe(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := bus.Subscribe(ctx, EventFilter{Types: []EventType{EventNodeComplete}})
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	_ = bus.Publish(ctx, Event{Type: EventNodeStart, NodeID: "n1"})
	_ = bus.Publish(ctx, Event{Type: EventNodeComplete, NodeID: "n1"})

	select {
	case ev := <-sub.Events():
		if ev.Type != EventNodeComplete {
			t.Fatalf("expected node.complete, got %s", ev.Type)
		}
		if ev.NodeID != "n1" {
			t.Fatalf("expected n1, got %s", ev.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestMemoryBus_FilterByRunID(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, _ := bus.Subscribe(ctx, EventFilter{RunID: "run-1"})

	_ = bus.Publish(ctx, Event{Type: EventNodeStart, RunID: "run-2"})
	_ = bus.Publish(ctx, Event{Type: EventNodeStart, RunID: "run-1"})

	select {
	case ev := <-sub.Events():
		if ev.RunID != "run-1" {
			t.Fatalf("expected run-1, got %s", ev.RunID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestMemoryBus_CtxCancel_AutoCleanup(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	sub, _ := bus.Subscribe(ctx, EventFilter{})

	cancel()
	time.Sleep(50 * time.Millisecond)

	_, open := <-sub.Events()
	if open {
		t.Fatal("channel should be closed after ctx cancel")
	}
}

func TestMemoryBus_Close_ClosesAll(t *testing.T) {
	bus := NewMemoryBus()
	ctx := context.Background()

	sub1, _ := bus.Subscribe(ctx, EventFilter{})
	sub2, _ := bus.Subscribe(ctx, EventFilter{})

	if err := bus.Close(); err != nil {
		t.Fatalf("close bus: %v", err)
	}

	_, open1 := <-sub1.Events()
	_, open2 := <-sub2.Events()
	if open1 || open2 {
		t.Fatal("all subscriptions should be closed")
	}
}

func TestMemoryBus_NonBlocking(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, EventFilter{})

	for i := 0; i < defaultBufferSize+10; i++ {
		err := bus.Publish(ctx, Event{Type: EventNodeStart})
		if err != nil {
			t.Fatalf("publish should not fail even when buffer full: %v", err)
		}
	}
}

func TestMemoryBus_FilterByActorID(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()

	sub, _ := bus.Subscribe(ctx, EventFilter{ActorID: "sess1:agent-a"})

	_ = bus.Publish(ctx, Event{Type: EventNodeStart, ActorID: "sess1:agent-b"})
	_ = bus.Publish(ctx, Event{Type: EventNodeComplete, ActorID: "sess1:agent-a"})

	select {
	case ev := <-sub.Events():
		if ev.ActorID != "sess1:agent-a" {
			t.Fatalf("expected sess1:agent-a, got %s", ev.ActorID)
		}
		if ev.Type != EventNodeComplete {
			t.Fatalf("expected node.complete, got %s", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestMemoryBus_EmptyActorID_MatchesAll(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()

	sub, _ := bus.Subscribe(ctx, EventFilter{})

	_ = bus.Publish(ctx, Event{Type: EventNodeStart, ActorID: "sess1:agent-a"})
	_ = bus.Publish(ctx, Event{Type: EventKanbanUpdate})

	received := 0
	timeout := time.After(time.Second)
	for received < 2 {
		select {
		case <-sub.Events():
			received++
		case <-timeout:
			t.Fatalf("expected 2 events, got %d", received)
		}
	}
}

func TestMemoryBus_KanbanEvents_AlwaysVisible(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()

	sub, _ := bus.Subscribe(ctx, EventFilter{ActorID: "sess1:agent-a"})

	_ = bus.Publish(ctx, Event{Type: EventKanbanUpdate})

	select {
	case ev := <-sub.Events():
		if ev.Type != EventKanbanUpdate {
			t.Fatalf("expected kanban.update, got %s", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("kanban event should be visible to ActorID-filtered subscriber")
	}
}

func TestMemoryBus_WithBufferSize(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	bigBuffer := 256
	sub, err := bus.Subscribe(ctx, EventFilter{}, WithBufferSize(bigBuffer))
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	// Publish more events than default buffer (64) but within our custom buffer (256).
	// With default buffer, events beyond 64 would be dropped.
	sent := defaultBufferSize + 50
	for i := 0; i < sent; i++ {
		_ = bus.Publish(ctx, Event{Type: EventNodeStart})
	}

	received := 0
	for {
		select {
		case <-sub.Events():
			received++
			if received >= sent {
				goto done
			}
		default:
			goto done
		}
	}
done:
	if received != sent {
		t.Fatalf("expected %d events with buffer size %d, got %d (default buffer would drop)", sent, bigBuffer, received)
	}
}

func TestMemoryBus_WithBufferSize_ZeroIgnored(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	// WithBufferSize(0) should be ignored, falling back to default
	sub, err := bus.Subscribe(ctx, EventFilter{}, WithBufferSize(0))
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	for i := 0; i < defaultBufferSize+10; i++ {
		_ = bus.Publish(ctx, Event{Type: EventNodeStart})
	}

	received := 0
	for {
		select {
		case <-sub.Events():
			received++
		default:
			goto done
		}
	}
done:
	if received != defaultBufferSize {
		t.Fatalf("expected exactly %d (default buffer), got %d", defaultBufferSize, received)
	}
}

func TestNoopBus(t *testing.T) {
	bus := NoopBus{}
	ctx := context.Background()

	if err := bus.Publish(ctx, Event{}); err != nil {
		t.Fatalf("noop publish should not fail: %v", err)
	}

	sub, _ := bus.Subscribe(ctx, EventFilter{})
	_, open := <-sub.Events()
	if open {
		t.Fatal("noop sub channel should be closed")
	}
}

func TestMemoryBus_DroppedCount(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, EventFilter{}, WithBufferSize(2))

	for i := 0; i < 10; i++ {
		_ = bus.Publish(ctx, Event{Type: EventNodeStart})
	}

	dropped := bus.Dropped()
	if dropped != 8 {
		t.Fatalf("expected 8 dropped events, got %d", dropped)
	}
}

func TestMemoryBus_DropCallback(t *testing.T) {
	var callbackCount atomic.Int64
	var lastDroppedType EventType

	bus := NewMemoryBus(WithDropCallback(func(ev Event) {
		callbackCount.Add(1)
		lastDroppedType = ev.Type
	}))
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, EventFilter{}, WithBufferSize(1))

	_ = bus.Publish(ctx, Event{Type: EventNodeStart})
	_ = bus.Publish(ctx, Event{Type: EventNodeComplete})
	_ = bus.Publish(ctx, Event{Type: EventNodeError})

	if callbackCount.Load() != 2 {
		t.Fatalf("expected 2 drop callbacks, got %d", callbackCount.Load())
	}
	if lastDroppedType != EventNodeError {
		t.Fatalf("expected last dropped type EventNodeError, got %s", lastDroppedType)
	}
}

func TestMemoryBus_DroppedZeroWhenNotFull(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	sub, _ := bus.Subscribe(ctx, EventFilter{}, WithBufferSize(64))

	_ = bus.Publish(ctx, Event{Type: EventNodeStart})

	if bus.Dropped() != 0 {
		t.Fatalf("expected 0 dropped, got %d", bus.Dropped())
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != EventNodeStart {
			t.Fatalf("expected EventNodeStart, got %s", ev.Type)
		}
	default:
		t.Fatal("expected event in channel")
	}
}

func TestMemoryBus_SubscriberMapRemoval(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx := context.Background()

	subs := make([]Subscription, 10)
	for i := range subs {
		var err error
		subs[i], err = bus.Subscribe(ctx, EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
	}

	bus.mu.RLock()
	if len(bus.subscribers) != 10 {
		t.Fatalf("expected 10 subscribers, got %d", len(bus.subscribers))
	}
	bus.mu.RUnlock()

	for i := 0; i < 5; i++ {
		_ = subs[i].Close()
	}

	bus.mu.RLock()
	if len(bus.subscribers) != 5 {
		t.Fatalf("expected 5 subscribers after closing 5, got %d", len(bus.subscribers))
	}
	bus.mu.RUnlock()
}

func TestMemoryBus_MultipleDropCallbacks_Concurrent(t *testing.T) {
	var dropped atomic.Int64
	bus := NewMemoryBus(WithDropCallback(func(_ Event) {
		dropped.Add(1)
	}))
	defer func() { _ = bus.Close() }()

	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, EventFilter{}, WithBufferSize(1))

	for i := 0; i < 100; i++ {
		_ = bus.Publish(ctx, Event{Type: EventNodeStart})
	}

	if dropped.Load() != 99 {
		t.Fatalf("expected 99 dropped, got %d", dropped.Load())
	}
	if bus.Dropped() != 99 {
		t.Fatalf("expected Dropped()=99, got %d", bus.Dropped())
	}
}
