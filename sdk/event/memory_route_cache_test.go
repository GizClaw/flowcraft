package event

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cacheLen is a test-only helper that reaches into MemoryBus's route
// cache. Living in the same package keeps it private to the test binary.
func cacheLen(b *MemoryBus) int {
	b.routeCacheMu.RLock()
	defer b.routeCacheMu.RUnlock()
	return len(b.routeCache)
}

func cacheHas(b *MemoryBus, subj Subject) bool {
	b.routeCacheMu.RLock()
	defer b.routeCacheMu.RUnlock()
	_, ok := b.routeCache[subj]
	return ok
}

// TestRouteCache_HitPopulates verifies that publishing populates the
// cache and that a second publish to the same subject reads back the
// same entry (i.e. the slow path is taken at most once).
func TestRouteCache_HitPopulates(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	if _, err := bus.Subscribe(ctx, "graph.run.r1.>"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if cacheLen(bus) != 0 {
		t.Fatalf("cache must start empty, got %d", cacheLen(bus))
	}
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.start", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !cacheHas(bus, "graph.run.r1.start") {
		t.Fatalf("cache should hold graph.run.r1.start after first publish")
	}

	// Second publish — same subject. Must reuse the cached slice. We
	// can only observe this indirectly by ensuring nothing breaks, but
	// the cache size must not grow.
	before := cacheLen(bus)
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.start", nil)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if cacheLen(bus) != before {
		t.Fatalf("cache size changed on hit: %d → %d", before, cacheLen(bus))
	}
}

// TestRouteCache_InvalidatedOnSubscribe is the most important regression:
// after a Subscribe call introduces a new pattern, every previously cached
// subject must be re-evaluated so the new subscription is not missed.
func TestRouteCache_InvalidatedOnSubscribe(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	subA, err := bus.Subscribe(ctx, "graph.run.r1.>")
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	// Warm the cache for the subject we will publish later.
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.start", nil)); err != nil {
		t.Fatalf("warm publish: %v", err)
	}
	if !cacheHas(bus, "graph.run.r1.start") {
		t.Fatalf("cache should be warm")
	}
	_ = drain(t, subA.C(), 1, time.Second)

	// Now add a SECOND subscription that also matches the warmed subject.
	subB, err := bus.Subscribe(ctx, "graph.>")
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	if cacheHas(bus, "graph.run.r1.start") {
		t.Fatalf("Subscribe must invalidate route cache; entry still present")
	}

	// Re-publish — the new subscription must receive it.
	if err := bus.Publish(ctx, mustEnv(t, "graph.run.r1.start", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	gotA := drain(t, subA.C(), 1, time.Second)
	gotB := drain(t, subB.C(), 1, time.Second)
	if len(gotA) != 1 || len(gotB) != 1 {
		t.Fatalf("after Subscribe+Publish: A=%d B=%d (want 1/1)", len(gotA), len(gotB))
	}
}

// TestRouteCache_InvalidatedOnUnsubscribe ensures a removed subscription
// no longer receives events from a subject that was cached before its
// removal. Using Block backpressure with no reader would deadlock, so we
// stick with DropNewest and check delivery counts via an Observer.
func TestRouteCache_InvalidatedOnUnsubscribe(t *testing.T) {
	var deliveredToA atomic.Int64
	var subAID SubscriptionID

	bus := NewMemoryBus(WithObserver(observerFunc{
		onDeliver: func(id SubscriptionID, _ Envelope) {
			if id == subAID {
				deliveredToA.Add(1)
			}
		},
	}))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	subA, err := bus.Subscribe(ctx, "kanban.>")
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	subAID = subA.ID()

	// Warm: A receives.
	if err := bus.Publish(ctx, mustEnv(t, "kanban.task.x.submitted", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = drain(t, subA.C(), 1, time.Second)
	if deliveredToA.Load() != 1 {
		t.Fatalf("A should have received 1, got %d", deliveredToA.Load())
	}

	// Unsubscribe A; cache must drop the entry so the next publish does
	// NOT see A in its cached route.
	if err := subA.Close(); err != nil {
		t.Fatalf("close subA: %v", err)
	}
	if cacheHas(bus, "kanban.task.x.submitted") {
		t.Fatalf("subA.Close must invalidate route cache")
	}

	if err := bus.Publish(ctx, mustEnv(t, "kanban.task.x.submitted", nil)); err != nil {
		t.Fatalf("publish post-close: %v", err)
	}
	// Allow OnDeliver goroutines (none expected) to settle.
	time.Sleep(20 * time.Millisecond)
	if deliveredToA.Load() != 1 {
		t.Fatalf("A must not receive after Close; got %d total deliveries", deliveredToA.Load())
	}
}

// TestRouteCache_PredicateNotCached guarantees that WithPredicate is
// re-evaluated on every Publish even when the subject route is cached.
// A cache that memoised the post-predicate result would deliver the
// second envelope incorrectly.
func TestRouteCache_PredicateNotCached(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	var allow atomic.Bool
	allow.Store(true)
	sub, err := bus.Subscribe(ctx, "x.>",
		WithPredicate(func(_ Envelope) bool { return allow.Load() }),
	)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := bus.Publish(ctx, mustEnv(t, "x.y", nil)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	got := drain(t, sub.C(), 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("predicate=true must deliver, got %d", len(got))
	}

	allow.Store(false)
	if err := bus.Publish(ctx, mustEnv(t, "x.y", nil)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	got2 := drain(t, sub.C(), 1, 100*time.Millisecond)
	if len(got2) != 0 {
		t.Fatalf("predicate=false must drop even on cache hit, got %d", len(got2))
	}
}

// TestRouteCache_CapEviction confirms the poor-man's LRU: once the cache
// hits its cap it is wholesale cleared, and further publishes still work.
func TestRouteCache_CapEviction(t *testing.T) {
	const cap = 4
	bus := NewMemoryBus(WithRouteCacheSize(cap))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	if _, err := bus.Subscribe(ctx, "k.>"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Publish cap distinct subjects — should fill the cache exactly.
	for i := 0; i < cap; i++ {
		s := Subject(fmt.Sprintf("k.s%d", i))
		if err := bus.Publish(ctx, mustEnv(t, s, nil)); err != nil {
			t.Fatalf("publish %s: %v", s, err)
		}
	}
	if cacheLen(bus) != cap {
		t.Fatalf("cache should be full at cap=%d, got %d", cap, cacheLen(bus))
	}

	// One more — must trigger wholesale clear, then store the new entry.
	if err := bus.Publish(ctx, mustEnv(t, "k.overflow", nil)); err != nil {
		t.Fatalf("publish overflow: %v", err)
	}
	if cacheLen(bus) != 1 {
		t.Fatalf("after overflow cache must contain only the new entry, got %d", cacheLen(bus))
	}
	if !cacheHas(bus, "k.overflow") {
		t.Fatalf("post-clear entry missing")
	}
}

// TestRouteCache_Disabled verifies WithRouteCacheSize(0) bypasses the
// cache entirely and Publish still delivers correctly.
func TestRouteCache_Disabled(t *testing.T) {
	bus := NewMemoryBus(WithRouteCacheSize(0))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, "x.>")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := bus.Publish(ctx, mustEnv(t, "x.y", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got := drain(t, sub.C(), 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("delivery broken with cache disabled, got %d", len(got))
	}
	if cacheLen(bus) != 0 {
		t.Fatalf("disabled cache must stay empty, got %d", cacheLen(bus))
	}
}

// TestRouteCache_NegativeSizeIgnored covers the WithRouteCacheSize n<0
// branch: should fall through to default (cache enabled).
func TestRouteCache_NegativeSizeIgnored(t *testing.T) {
	bus := NewMemoryBus(WithRouteCacheSize(-1))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	if _, err := bus.Subscribe(ctx, "x.>"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := bus.Publish(ctx, mustEnv(t, "x.y", nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if cacheLen(bus) == 0 {
		t.Fatalf("negative size should fall through to default; cache not populated")
	}
}

// TestRouteCache_ConcurrentSubscribePublish stresses the
// b.mu↔routeCacheMu lock ordering. With -race this catches both deadlock
// and torn cache reads. We continuously add/remove subscriptions while
// publishing on a separate goroutine; the only invariant we assert is
// "no panic, no deadlock, every delivered envelope was meant for the
// destination subscription".
func TestRouteCache_ConcurrentSubscribePublish(t *testing.T) {
	bus := NewMemoryBus(WithRouteCacheSize(64))
	defer func() { _ = bus.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		publishers = 4
		churners   = 2
		duration   = 200 * time.Millisecond
	)

	stop := make(chan struct{})
	time.AfterFunc(duration, func() { close(stop) })

	var wg sync.WaitGroup

	// Long-lived sink so publishes have at least one stable subscriber.
	sink, err := bus.Subscribe(ctx, "test.>")
	if err != nil {
		t.Fatalf("sink subscribe: %v", err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case env, ok := <-sink.C():
				if !ok {
					return
				}
				// Mismatched route would surface here.
				if len(env.Subject) < 5 || env.Subject[:5] != "test." {
					t.Errorf("sink got wrong subject %q", env.Subject)
					return
				}
			}
		}
	}()

	for i := 0; i < publishers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				subj := Subject(fmt.Sprintf("test.p%d.n%d", i, n%8))
				_ = bus.Publish(ctx, mustEnv(t, subj, nil))
			}
		}()
	}

	for i := 0; i < churners; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				pat := Pattern(fmt.Sprintf("test.p%d.>", i))
				s, err := bus.Subscribe(ctx, pat)
				if err != nil {
					return
				}
				time.Sleep(time.Microsecond * 50)
				_ = s.Close()
			}
		}()
	}

	wg.Wait()
}

// observerFunc adapts function fields into the Observer interface for
// concise table tests.
type observerFunc struct {
	onPublish func(Envelope)
	onDeliver func(SubscriptionID, Envelope)
	onDrop    func(SubscriptionID, Envelope, DropReason)
}

func (o observerFunc) OnPublish(env Envelope) {
	if o.onPublish != nil {
		o.onPublish(env)
	}
}
func (o observerFunc) OnDeliver(id SubscriptionID, env Envelope) {
	if o.onDeliver != nil {
		o.onDeliver(id, env)
	}
}
func (o observerFunc) OnDrop(id SubscriptionID, env Envelope, r DropReason) {
	if o.onDrop != nil {
		o.onDrop(id, env, r)
	}
}
