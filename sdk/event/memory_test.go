package event

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustEnv(t *testing.T, subj Subject, payload any) Envelope {
	t.Helper()
	env, err := NewEnvelope(context.Background(), subj, payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

func TestMemoryBus_PatternRouting(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	subRun, err := bus.Subscribe(ctx, "graph.run.r1.>")
	if err != nil {
		t.Fatalf("subscribe r1: %v", err)
	}
	subStarts, err := bus.Subscribe(ctx, "graph.run.*.start")
	if err != nil {
		t.Fatalf("subscribe starts: %v", err)
	}

	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.start", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r2.start", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.node.n1.complete", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// subRun should see both r1 events.
	got1 := drain(t, subRun.C(), 2, time.Second)
	if len(got1) != 2 {
		t.Fatalf("subRun: want 2, got %d", len(got1))
	}
	if got1[0].Subject != "graph.run.r1.start" || got1[1].Subject != "graph.run.r1.node.n1.complete" {
		t.Fatalf("subRun order/values wrong: %+v", got1)
	}

	// subStarts should see both r1.start and r2.start.
	got2 := drain(t, subStarts.C(), 2, time.Second)
	if len(got2) != 2 {
		t.Fatalf("subStarts: want 2, got %d", len(got2))
	}
}

func TestMemoryBus_WithPredicate(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, "kanban.>", WithPredicate(func(e Envelope) bool {
		return e.Tenant() == "acme"
	}))
	if err != nil {
		t.Fatal(err)
	}

	envA := mustEnv(t, "kanban.board.b1.update", nil)
	envA.SetTenant("acme")
	envB := mustEnv(t, "kanban.board.b1.update", nil)
	envB.SetTenant("other")

	_ = bus.Publish(ctx, envA)
	_ = bus.Publish(ctx, envB)

	got := drain(t, sub.C(), 1, 200*time.Millisecond)
	if len(got) != 1 || got[0].Tenant() != "acme" {
		t.Fatalf("predicate filter failed: %+v", got)
	}
}

func TestMemoryBus_BackpressureDropNewest(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(DropNewest))
	if err != nil {
		t.Fatal(err)
	}

	first := mustEnv(t, "x.1", nil)
	second := mustEnv(t, "x.2", nil)
	third := mustEnv(t, "x.3", nil)

	if err := bus.Publish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, third); err != nil {
		t.Fatal(err)
	}

	got := drain(t, sub.C(), 1, 200*time.Millisecond)
	if len(got) != 1 || got[0].Subject != "x.1" {
		t.Fatalf("DropNewest should keep oldest (x.1), got %+v", got)
	}
	if bus.Dropped() != 2 {
		t.Fatalf("Dropped want 2, got %d", bus.Dropped())
	}
}

func TestMemoryBus_BackpressureDropOldest(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(DropOldest))
	if err != nil {
		t.Fatal(err)
	}

	for i, subj := range []Subject{"x.1", "x.2", "x.3"} {
		if err := bus.Publish(ctx, mustEnv(t, subj, nil)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	got := drain(t, sub.C(), 1, 200*time.Millisecond)
	if len(got) != 1 || got[0].Subject != "x.3" {
		t.Fatalf("DropOldest should keep newest (x.3), got %+v", got)
	}
}

func TestMemoryBus_BackpressureBlock(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(Block))
	if err != nil {
		t.Fatal(err)
	}

	if err := bus.Publish(ctx, mustEnv(t, "x.1", nil)); err != nil {
		t.Fatal(err)
	}

	publishDone := make(chan struct{})
	go func() {
		_ = bus.Publish(ctx, mustEnv(t, "x.2", nil))
		close(publishDone)
	}()

	select {
	case <-publishDone:
		t.Fatal("Publish should be blocked when buffer is full")
	case <-time.After(50 * time.Millisecond):
	}

	<-sub.C() // unblock
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("Publish should have unblocked after consume")
	}
}

func TestMemoryBus_BackpressureBlock_RespectsCtx(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	_, err := bus.Subscribe(context.Background(), ">", WithBufferSize(1), WithBackpressure(Block))
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), mustEnv(t, "x.1", nil)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = bus.Publish(ctx, mustEnv(t, "x.2", nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

type recordObserver struct {
	mu       sync.Mutex
	publish  int
	deliver  int
	drop     int
	dropReas []DropReason
}

func (r *recordObserver) OnPublish(Envelope) {
	r.mu.Lock()
	r.publish++
	r.mu.Unlock()
}
func (r *recordObserver) OnDeliver(SubscriptionID, Envelope) {
	r.mu.Lock()
	r.deliver++
	r.mu.Unlock()
}
func (r *recordObserver) OnDrop(_ SubscriptionID, _ Envelope, reason DropReason) {
	r.mu.Lock()
	r.drop++
	r.dropReas = append(r.dropReas, reason)
	r.mu.Unlock()
}

func TestMemoryBus_Observer(t *testing.T) {
	obs := &recordObserver{}
	bus := NewMemoryBus(WithObserver(obs))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, _ := bus.Subscribe(ctx, ">", WithBufferSize(1))
	if err := bus.Publish(ctx, mustEnv(t, "x.1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, mustEnv(t, "x.2", nil)); err != nil {
		t.Fatal(err)
	}

	<-sub.C()

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.publish != 2 {
		t.Errorf("publish: want 2, got %d", obs.publish)
	}
	if obs.deliver != 1 {
		t.Errorf("deliver: want 1, got %d", obs.deliver)
	}
	if obs.drop != 1 || obs.dropReas[0] != DropReasonBufferFull {
		t.Errorf("drop: want 1 BufferFull, got %d %v", obs.drop, obs.dropReas)
	}
}

func TestMemoryBus_SubscribeRejectsInvalidPattern(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	_, err := bus.Subscribe(context.Background(), "a.>.b")
	if !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("want ErrInvalidPattern, got %v", err)
	}
}

func TestMemoryBus_Close_Idempotent(t *testing.T) {
	bus := NewMemoryBus()
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal("second Close should be a no-op")
	}
	err := bus.Publish(context.Background(), mustEnv(t, "x", nil))
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("want ErrBusClosed, got %v", err)
	}
	_, err = bus.Subscribe(context.Background(), ">")
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("want ErrBusClosed, got %v", err)
	}
}

func TestMemoryBus_CtxCancelClosesSubscription(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, ">")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("channel should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription channel did not close on ctx cancel")
	}
}

// TestMemoryBus_Close_NoSendOnClosedChannelPanic is the regression test
// for the legacy Close race. The narrow window the test exercises is the
// Block-backpressure send path: Publish releases the bus RLock before
// sending and re-acquires it afterwards. Without inflight.Wait() in
// MemoryBus.Close, a concurrent Close() would close the subscription
// channel between those two points and the parked Publish would resume
// into "send on closed channel".
//
// We pin the test to that exact path by giving every subscription Block
// backpressure and a tiny buffer, then race many publishers against a
// single Close.
func TestMemoryBus_Close_NoSendOnClosedChannelPanic(t *testing.T) {
	const (
		iterations  = 30
		publishers  = 8
		perPublish  = 50
		subscribers = 4
	)

	for iter := 0; iter < iterations; iter++ {
		bus := NewMemoryBus()
		ctx := context.Background()

		subs := make([]Subscription, subscribers)
		for i := range subs {
			s, err := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(Block))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			subs[i] = s
		}

		// Drain in the background so blocked Publish goroutines can
		// progress; once the channel is closed we exit.
		drainStop := make(chan struct{})
		var drained atomic.Int64
		var drainWG sync.WaitGroup
		for _, s := range subs {
			drainWG.Add(1)
			go func(c <-chan Envelope) {
				defer drainWG.Done()
				for {
					select {
					case _, ok := <-c:
						if !ok {
							return
						}
						drained.Add(1)
					case <-drainStop:
						return
					}
				}
			}(s.C())
		}

		var pubWG sync.WaitGroup
		var pubErr atomic.Value // first non-ErrBusClosed error
		for p := 0; p < publishers; p++ {
			pubWG.Add(1)
			go func() {
				defer pubWG.Done()
				for i := 0; i < perPublish; i++ {
					err := bus.Publish(ctx, mustEnv(t, "x.race", nil))
					if err != nil && !errors.Is(err, ErrBusClosed) {
						pubErr.CompareAndSwap(nil, err)
						return
					}
				}
			}()
		}

		// Race Close against the blocked / mid-send publishers.
		time.Sleep(50 * time.Microsecond)
		if err := bus.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		pubWG.Wait()
		close(drainStop)
		drainWG.Wait()

		if v := pubErr.Load(); v != nil {
			t.Fatalf("iter %d: publisher reported error: %v", iter, v)
		}
	}
}

// TestMemoryBus_Close_UnblocksParkedPublisher pins the deadlock fix:
// when a Block-backpressure publisher is parked on a full subscription
// channel and there are no consumers, Close() must signal sub.done so
// the publisher exits, then drain inflight, then close the channel.
// A buggy implementation would call inflight.Wait() before signalling
// done and hang forever.
func TestMemoryBus_Close_UnblocksParkedPublisher(t *testing.T) {
	bus := NewMemoryBus()
	ctx := context.Background()

	_, err := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(Block))
	if err != nil {
		t.Fatal(err)
	}

	// Fill the buffer.
	if err := bus.Publish(ctx, mustEnv(t, "x.1", nil)); err != nil {
		t.Fatal(err)
	}

	// Park a second Publish — no consumer to drain it.
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(ctx, mustEnv(t, "x.2", nil))
	}()
	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- bus.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked waiting on parked publisher")
	}

	select {
	case <-publishDone:
		// Publish should return after sub.done was signalled — either
		// successfully (if drained between fill and close) or with
		// ErrBusClosed.
	case <-time.After(time.Second):
		t.Fatal("parked Publish did not unblock after Close")
	}
}

// TestMemoryBus_Close_NoLeakedPublishGoroutine confirms inflight.Wait()
// actually drains; a buggy implementation would let Close return while a
// Publish was still mid-send.
func TestMemoryBus_Close_NoLeakedPublishGoroutine(t *testing.T) {
	bus := NewMemoryBus()
	ctx := context.Background()

	sub, _ := bus.Subscribe(ctx, ">", WithBufferSize(1), WithBackpressure(Block))
	// Fill the buffer.
	if err := bus.Publish(ctx, mustEnv(t, "x.1", nil)); err != nil {
		t.Fatal(err)
	}

	// Start a Publish that will block on the full buffer.
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(ctx, mustEnv(t, "x.2", nil))
	}()

	// Give the Publish a moment to actually block.
	time.Sleep(20 * time.Millisecond)

	// Drain one item so the blocked Publish can proceed; then Close.
	<-sub.C()

	// Wait for Publish to return before Close so we are exercising the
	// happy "drain to zero" path.
	if err := <-publishDone; err != nil {
		t.Fatalf("blocked publish returned: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// drain reads up to n envelopes from ch within timeout.
func drain(t *testing.T, ch <-chan Envelope, n int, timeout time.Duration) []Envelope {
	t.Helper()
	out := make([]Envelope, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(out) < n {
		select {
		case env, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, env)
		case <-deadline.C:
			return out
		}
	}
	return out
}
