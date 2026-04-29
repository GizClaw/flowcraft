package event

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel/trace"
)

// MemoryBusOption configures a MemoryBus at construction time.
type MemoryBusOption func(*MemoryBus)

// WithObserver attaches an Observer for lifecycle instrumentation.
//
// Replaces the legacy WithDropCallback option. See Observer for the
// concurrency contract.
func WithObserver(o Observer) MemoryBusOption {
	return func(b *MemoryBus) {
		if o != nil {
			b.observer = o
		}
	}
}

// WithRouteCacheSize bounds the subject→subscribers route cache.
//
// The route cache memoises the result of scanning every subscription for
// a given Subject so that hot subjects skip the per-publish O(N) match
// loop. Cache entries are invalidated whenever the subscription set
// changes (Subscribe / removeSub / Close).
//
// Sizing:
//   - n > 0  : cap at n distinct subjects; on overflow the cache is
//     wholesale cleared (poor-man's LRU — adequate when the working set
//     is small or churns slowly, which matches every current SDK
//     producer).
//   - n == 0 : disable the cache entirely; every Publish scans
//     subscribers. Use when subjects are unique-per-publish (e.g. ID
//     embedded in subject and never repeated).
//   - n < 0  : ignored; default applies.
//
// Default is defaultRouteCacheSize.
func WithRouteCacheSize(n int) MemoryBusOption {
	return func(b *MemoryBus) {
		if n < 0 {
			return
		}
		b.routeCacheCap = n
	}
}

// MemoryBus is the in-process Bus implementation.
//
// Concurrency model:
//   - subscribers map is guarded by mu;
//   - Publish takes RLock for the route-and-enqueue scan;
//   - Subscribe / Close / removeSub take the write lock;
//   - in-flight Publish calls are tracked by inflight (sync.WaitGroup) so
//     Close can wait for them to drain before any subscriber channel is
//     closed — this avoids the "send on closed channel" race that bit
//     earlier per-subscription bus implementations.
//
// Hot-path lock-freedom:
//   - Observer callbacks are deferred to a local slice and executed after
//     the RLock has been released.
//
// Route cache:
//   - routeCache memoises subject → []*memSub so a hot subject avoids the
//     O(N) match loop. Guarded by routeCacheMu (a separate mutex from
//     b.mu so cache writes don't compete with the publish RLock fan-out).
//   - Lock order when both are held: ALWAYS b.mu first, then
//     routeCacheMu. Subscribe / Close / removeSub clear the cache while
//     holding b.mu.Lock(); Publish reads/writes the cache while holding
//     b.mu.RLock(). This ordering prevents a writer's clear from racing
//     a publisher's lookup of stale entries.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[SubscriptionID]*memSub
	closed      bool
	dropped     atomic.Int64

	// inflight counts Publish calls currently between their wg.Add(1) and
	// matching wg.Done(). Close uses Wait() to block until every Publish
	// returns, so closing subscriber channels is safe.
	inflight sync.WaitGroup

	observer Observer
	subSeq   atomic.Uint64

	// Route cache. routeCacheCap == 0 disables the cache.
	routeCacheMu  sync.RWMutex
	routeCache    map[Subject][]*memSub
	routeCacheCap int
}

const (
	defaultMemoryBufferSize = 64
	defaultRouteCacheSize   = 1024
)

// NewMemoryBus constructs an in-process Bus.
func NewMemoryBus(opts ...MemoryBusOption) *MemoryBus {
	b := &MemoryBus{
		subscribers:   make(map[SubscriptionID]*memSub),
		observer:      noopObserver{},
		routeCacheCap: defaultRouteCacheSize,
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.routeCacheCap > 0 {
		b.routeCache = make(map[Subject][]*memSub, b.routeCacheCap)
	}
	return b
}

// lookupRoute returns the cached subscriber slice for subj or nil/false on
// miss. Caller must hold b.mu.RLock so the returned slice cannot be
// invalidated mid-scan.
func (b *MemoryBus) lookupRoute(subj Subject) ([]*memSub, bool) {
	if b.routeCacheCap == 0 {
		return nil, false
	}
	b.routeCacheMu.RLock()
	subs, ok := b.routeCache[subj]
	b.routeCacheMu.RUnlock()
	return subs, ok
}

// storeRoute writes the matched subscriber slice for subj. Caller must
// hold b.mu.RLock so subscribers cannot churn between match and store.
// On overflow the entire cache is cleared (poor-man's LRU).
func (b *MemoryBus) storeRoute(subj Subject, subs []*memSub) {
	if b.routeCacheCap == 0 {
		return
	}
	b.routeCacheMu.Lock()
	if len(b.routeCache) >= b.routeCacheCap {
		b.routeCache = make(map[Subject][]*memSub, b.routeCacheCap)
	}
	b.routeCache[subj] = subs
	b.routeCacheMu.Unlock()
}

// clearRouteCache drops every cached entry. Caller must hold b.mu.Lock
// (write lock) so no concurrent Publish can observe an inconsistent cache
// against a churning subscriber set.
func (b *MemoryBus) clearRouteCache() {
	if b.routeCacheCap == 0 {
		return
	}
	b.routeCacheMu.Lock()
	if len(b.routeCache) > 0 {
		b.routeCache = make(map[Subject][]*memSub, b.routeCacheCap)
	}
	b.routeCacheMu.Unlock()
}

// Dropped returns the cumulative count of envelopes dropped due to
// DropNewest / DropOldest policies. Diagnostic only.
func (b *MemoryBus) Dropped() int64 { return b.dropped.Load() }

// memSub is the in-memory subscription record.
type memSub struct {
	id           SubscriptionID
	pattern      Pattern
	patternSegs  []string // pre-split pattern; never mutated after Subscribe.
	predicate    func(Envelope) bool
	backpressure BackpressurePolicy
	sampleRate   float64
	ch           chan Envelope
	done         chan struct{}
	bus          *MemoryBus

	// signalOnce guards close(done); chanOnce guards close(ch). Two
	// separate Once values are needed because shutdown is split: bus.Close
	// closes done first (signalOnce), drains inflight + senders, then
	// closes ch (chanOnce); user-driven memSub.Close runs both phases
	// inline. Using a single Once would let one path skip the other.
	signalOnce sync.Once
	chanOnce   sync.Once

	// senders tracks Publish calls currently performing or about to
	// perform a `ch <- env` for this subscription. close(ch) must wait on
	// senders.Wait() before closing ch, otherwise a Block-backpressure
	// publisher parked on the channel can resume into a closed channel.
	// Only the Block path bumps this counter — the non-blocking
	// DropNewest / DropOldest paths complete their send while still
	// holding bus.mu.RLock and therefore cannot race with close(ch)
	// (which is called after bus.mu has been taken in write mode).
	senders sync.WaitGroup
}

func (s *memSub) ID() SubscriptionID { return s.id }
func (s *memSub) C() <-chan Envelope { return s.ch }

// Close is the user-facing path.
//
// Two-phase shutdown analogous to MemoryBus.Close, scoped to a single
// subscription:
//  1. removeSub takes the bus write lock — this excludes any new Publish
//     scan from observing this subscription. Publish goroutines already
//     past the scan but parked on a Block-backpressure send still hold a
//     pointer to s.ch.
//  2. close(s.done) wakes those parked sends; the Block branch also
//     selects on done and exits without sending.
//  3. s.senders.Wait() waits for every Block sender to call sub.senders.Done
//     (after either delivering, observing done, or being cancelled).
//  4. close(s.ch) is now race-free.
func (s *memSub) Close() error {
	if s.bus != nil {
		s.bus.removeSub(s.id)
	}
	s.signalOnce.Do(func() { close(s.done) })
	s.senders.Wait()
	s.chanOnce.Do(func() { close(s.ch) })
	return nil
}

// signalClose is invoked by MemoryBus.Close as the first step of the
// bus-wide shutdown. It only closes s.done so Block-backpressure
// publishers can exit; s.ch is closed later (closeChanFromBus) once the
// bus has drained both its global inflight wg and this sub's senders wg.
func (s *memSub) signalClose() {
	s.signalOnce.Do(func() { close(s.done) })
}

// closeChanFromBus closes s.ch. MUST be called only after both the
// parent Bus has drained inflight AND s.senders has drained.
func (s *memSub) closeChanFromBus() {
	s.chanOnce.Do(func() { close(s.ch) })
}

// deliverCb / dropCb are deferred observer callbacks collected during a
// Publish scan and fired after every bus lock has been released.
type deliverCb struct {
	subID SubscriptionID
}

type dropCb struct {
	subID  SubscriptionID
	reason DropReason
}

// patternMatches reports whether the subscription's pattern matches
// sSegs. Called under the bus RLock. Pattern matching is independent of
// envelope payload/headers so its result can be safely cached per
// Subject; user-supplied predicates cannot (they may inspect the
// envelope) and are evaluated separately by the publish loop.
func (s *memSub) patternMatches(sSegs []string) bool {
	return matchSegs(s.patternSegs, sSegs)
}

// predicateAllows runs the optional WithPredicate filter; returns true
// when the subscription has no predicate.
func (s *memSub) predicateAllows(env Envelope) bool {
	if s.predicate == nil {
		return true
	}
	return s.predicate(env)
}

// Publish routes env to every matching subscription according to each
// subscription's BackpressurePolicy.
func (b *MemoryBus) Publish(ctx context.Context, env Envelope) error {
	// Step 1: fast-fail if already closed (no inflight registration yet).
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrBusClosed
	}
	// Register inflight while still holding RLock so Close cannot observe
	// "closed=true and inflight=0" between the two checks.
	b.inflight.Add(1)
	b.mu.RUnlock()
	defer b.inflight.Done()

	if env.ID == "" {
		env.ID = xid.New().String()
	}
	if env.Time.IsZero() {
		env.Time = time.Now()
	}
	if env.TraceID == "" || env.SpanID == "" {
		if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
			if env.TraceID == "" {
				env.TraceID = sc.TraceID().String()
			}
			if env.SpanID == "" {
				env.SpanID = sc.SpanID().String()
			}
		}
	}

	// Collect observer callbacks while under RLock; invoke after release.
	var (
		delivers []deliverCb
		drops    []dropCb
	)

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrBusClosed
	}

	// Resolve the pattern-matching subscription set for this subject.
	// Cached value reflects PATTERN matches only; per-publish predicates
	// still run inside the loop because they may depend on payload /
	// headers and therefore cannot be memoised by Subject alone.
	matched, hit := b.lookupRoute(env.Subject)
	if !hit {
		sSegs := splitSubject(string(env.Subject))
		// Allocate a fresh slice — the result is published into the cache
		// and may be read concurrently by future Publish calls; reusing
		// any caller-side scratch buffer would corrupt entries.
		matched = matched[:0:0]
		for _, sub := range b.subscribers {
			if sub.patternMatches(sSegs) {
				matched = append(matched, sub)
			}
		}
		b.storeRoute(env.Subject, matched)
	}

	for _, sub := range matched {
		if !sub.predicateAllows(env) {
			continue
		}
		select {
		case <-sub.done:
			// Subscription closed concurrently — count as Closed drop.
			drops = append(drops, dropCb{sub.id, DropReasonClosed})
			continue
		default:
		}

		switch sub.backpressure {
		case Block:
			// Block path requires releasing the RLock so a parallel
			// subscription Close (which takes the write lock) cannot
			// deadlock against us. We snapshot the channel reference and
			// drop the lock for this single send.
			//
			// We register on sub.senders BEFORE releasing the bus RLock.
			// memSub.Close acquires the bus write lock first (excluding
			// any new entries here), then waits on senders before
			// closing sub.ch — so the registration here happens-before
			// any close(sub.ch) that could race the parked send below.
			sub.senders.Add(1)
			ch := sub.ch
			done := sub.done
			subID := sub.id
			b.mu.RUnlock()
			select {
			case ch <- env:
				delivers = append(delivers, deliverCb{subID})
			case <-done:
				drops = append(drops, dropCb{subID, DropReasonClosed})
			case <-ctx.Done():
				sub.senders.Done()
				// Re-acquire to keep the loop invariant before bailing.
				b.fireObserver(env, delivers, drops)
				return errdefs.FromContext(ctx.Err())
			}
			sub.senders.Done()
			b.mu.RLock()
			// b.closed may have flipped; if so, stop scanning.
			if b.closed {
				b.mu.RUnlock()
				b.fireObserver(env, delivers, drops)
				return ErrBusClosed
			}
			continue

		case DropOldest:
			// Try non-blocking send; on full, drop one oldest then retry.
			select {
			case sub.ch <- env:
				delivers = append(delivers, deliverCb{sub.id})
			default:
				select {
				case <-sub.ch:
					b.dropped.Add(1)
				default:
				}
				select {
				case sub.ch <- env:
					delivers = append(delivers, deliverCb{sub.id})
				default:
					// Buffer churn lost the race; count as full drop.
					b.dropped.Add(1)
					drops = append(drops, dropCb{sub.id, DropReasonBufferFull})
				}
			}

		case Sample:
			// Sampling decision happens BEFORE the buffer-full check so
			// the keep ratio is independent of subscriber speed: a fast
			// consumer and a slow one both see ~rate fraction of events.
			// Rate is clamped at construction time (WithSampleRate), so
			// rate==1 short-circuits to a plain non-blocking send and
			// rate==0 drops every envelope.
			keep := sub.sampleRate >= 1 || (sub.sampleRate > 0 && rand.Float64() < sub.sampleRate)
			if !keep {
				b.dropped.Add(1)
				drops = append(drops, dropCb{sub.id, DropReasonSampled})
				continue
			}
			select {
			case sub.ch <- env:
				delivers = append(delivers, deliverCb{sub.id})
			default:
				b.dropped.Add(1)
				drops = append(drops, dropCb{sub.id, DropReasonBufferFull})
			}

		default: // DropNewest
			select {
			case sub.ch <- env:
				delivers = append(delivers, deliverCb{sub.id})
			default:
				b.dropped.Add(1)
				drops = append(drops, dropCb{sub.id, DropReasonBufferFull})
			}
		}
	}
	b.mu.RUnlock()

	b.fireObserver(env, delivers, drops)
	return nil
}

// fireObserver invokes the configured observer outside any bus lock.
func (b *MemoryBus) fireObserver(env Envelope, delivers []deliverCb, drops []dropCb) {
	if b.observer == nil {
		return
	}
	b.observer.OnPublish(env)
	for _, d := range delivers {
		b.observer.OnDeliver(d.subID, env)
	}
	for _, d := range drops {
		b.observer.OnDrop(d.subID, env, d.reason)
	}
}

// Subscribe creates a subscription. pattern is validated; ctx cancellation
// triggers Close.
func (b *MemoryBus) Subscribe(ctx context.Context, pattern Pattern, opts ...SubOption) (Subscription, error) {
	if err := pattern.Validate(); err != nil {
		return nil, err
	}

	o := subOptions{
		bufferSize:   defaultMemoryBufferSize,
		backpressure: DropNewest,
		sampleRate:   1.0,
	}
	for _, fn := range opts {
		fn(&o)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBusClosed
	}

	// Cache invalidation: any subscription churn changes the pattern set
	// and therefore every cached subject→subs entry. Must run while
	// holding the write lock so concurrent Publish RLock holders cannot
	// race against a half-updated cache.
	b.clearRouteCache()

	sub := &memSub{
		id:           SubscriptionID(fmt.Sprintf("ms-%d", b.subSeq.Add(1))),
		pattern:      pattern,
		patternSegs:  splitSubject(string(pattern)),
		predicate:    o.predicate,
		backpressure: o.backpressure,
		sampleRate:   o.sampleRate,
		ch:           make(chan Envelope, o.bufferSize),
		done:         make(chan struct{}),
		bus:          b,
	}
	b.subscribers[sub.id] = sub
	b.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			_ = sub.Close()
		case <-sub.done:
		}
	}()

	return sub, nil
}

// Close performs a multi-phase shutdown:
//
//  1. Take the write lock, mark closed, snapshot subscribers, release lock.
//     New Publish / Subscribe calls now fail with ErrBusClosed.
//  2. Wake every Block-backpressure publisher by closing each sub.done
//     (signalClose). They will exit with a DropReasonClosed drop.
//  3. Wait for every in-flight Publish to return — both publishers
//     parked in Block sends and publishers in the middle of a
//     non-blocking enqueue.
//  4. Wait for every sub's per-sub senders wg to drain (defence in
//     depth: any still-parked Block send must call senders.Done before
//     we close the channel).
//  5. Close every subscription channel.
//
// Without step 2, step 3 (inflight.Wait) would deadlock against any
// Block publisher whose target channel is full and whose subscription
// has no remaining consumer.
func (b *MemoryBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := b.subscribers
	b.subscribers = make(map[SubscriptionID]*memSub)
	b.clearRouteCache()
	b.mu.Unlock()

	for _, sub := range subs {
		sub.signalClose()
	}

	b.inflight.Wait()

	for _, sub := range subs {
		sub.senders.Wait()
		sub.closeChanFromBus()
	}
	return nil
}

func (b *MemoryBus) removeSub(id SubscriptionID) {
	b.mu.Lock()
	if _, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		b.clearRouteCache()
	}
	b.mu.Unlock()
}
