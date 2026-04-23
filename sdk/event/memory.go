package event

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/xid"
	"go.opentelemetry.io/otel/trace"
)

// ErrBusClosed is returned by Publish / Subscribe after a Bus has been closed.
var ErrBusClosed = errors.New("event: bus closed")

// Bus is a publish-subscribe channel for Envelopes routed by Subject /
// Pattern.
//
// Bus is the new (post v0.1.x) primary surface; the legacy Event /
// LegacyEventBus types remain available as deprecated shims for one
// minor cycle and will be removed in v0.2.0.
type Bus interface {
	// Publish delivers env to every matching subscription. Implementations
	// must:
	//   - fill env.ID and env.Time when zero;
	//   - return ErrBusClosed once Close has been invoked;
	//   - guarantee that, on return, the envelope has been enqueued for
	//     every matching subscription, or dropped according to that
	//     subscription's BackpressurePolicy.
	//
	// Cross-process implementations (added in later commits) may relax
	// the on-return delivery guarantee to "fire-and-forget"; that
	// relaxation must be documented at the implementation level.
	Publish(ctx context.Context, env Envelope) error

	// Subscribe creates a new subscription matching pattern. The returned
	// Subscription is closed automatically when ctx is cancelled.
	// pattern is validated and a malformed value returns an error.
	Subscribe(ctx context.Context, pattern Pattern, opts ...SubOption) (Subscription, error)

	// Close shuts down the bus. After Close returns, every subscription
	// channel will be closed and subsequent Publish / Subscribe calls
	// return ErrBusClosed. Close is idempotent.
	Close() error
}

// Subscription is an active receive-side handle.
type Subscription interface {
	// ID is unique within the originating bus instance.
	ID() SubscriptionID
	// C returns the envelope channel. The channel is closed exactly once,
	// either when Close is invoked or when the parent bus is closed.
	C() <-chan Envelope
	// Close cancels the subscription. Idempotent.
	Close() error
}

// SubOption configures a Subscription at creation time.
type SubOption func(*subOptions)

type subOptions struct {
	bufferSize   int
	backpressure BackpressurePolicy
	predicate    func(Envelope) bool
}

// WithBufferSize sets the buffered channel capacity. Values <= 0 fall back
// to the default (64).
func WithBufferSize(n int) SubOption {
	return func(o *subOptions) {
		if n > 0 {
			o.bufferSize = n
		}
	}
}

// WithBackpressure overrides the default DropNewest policy.
func WithBackpressure(p BackpressurePolicy) SubOption {
	return func(o *subOptions) { o.backpressure = p }
}

// WithPredicate adds a secondary filter applied after pattern matching.
// Useful when subject routing alone cannot express the desired filter
// (e.g. multi-tenant header checks).
func WithPredicate(fn func(Envelope) bool) SubOption {
	return func(o *subOptions) { o.predicate = fn }
}

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

// MemoryBus is the in-process Bus implementation.
//
// Concurrency model:
//   - subscribers map is guarded by mu;
//   - Publish takes RLock for the route-and-enqueue scan;
//   - Subscribe / Close / removeSub take the write lock;
//   - in-flight Publish calls are tracked by inflight (sync.WaitGroup) so
//     Close can wait for them to drain before any subscriber channel is
//     closed — this is the fix for the legacy "send on closed channel"
//     race documented in commit 1's predecessor (LegacyMemoryBus).
//
// Hot-path lock-freedom:
//   - Observer callbacks are deferred to a local slice and executed after
//     the RLock has been released.
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
}

const defaultMemoryBufferSize = 64

// NewMemoryBus constructs an in-process Bus.
func NewMemoryBus(opts ...MemoryBusOption) *MemoryBus {
	b := &MemoryBus{
		subscribers: make(map[SubscriptionID]*memSub),
		observer:    noopObserver{},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Dropped returns the cumulative count of envelopes dropped due to
// DropNewest / DropOldest policies. Diagnostic only.
func (b *MemoryBus) Dropped() int64 { return b.dropped.Load() }

// NoopBus is a Bus that discards every Publish and yields a closed channel
// for every Subscribe. Useful for tests and as a default value to keep
// nil-checks out of producer code.
type NoopBus struct{}

// NewNoopBus returns a NoopBus value (kept for symmetry with NewMemoryBus).
func NewNoopBus() NoopBus { return NoopBus{} }

func (NoopBus) Publish(context.Context, Envelope) error { return nil }
func (NoopBus) Subscribe(context.Context, Pattern, ...SubOption) (Subscription, error) {
	return noopSubscription{}, nil
}
func (NoopBus) Close() error { return nil }

var noopClosedCh = func() chan Envelope {
	ch := make(chan Envelope)
	close(ch)
	return ch
}()

type noopSubscription struct{}

func (noopSubscription) ID() SubscriptionID { return "noop" }
func (noopSubscription) C() <-chan Envelope { return noopClosedCh }
func (noopSubscription) Close() error       { return nil }

// memSub is the in-memory subscription record.
type memSub struct {
	id           SubscriptionID
	pattern      Pattern
	predicate    func(Envelope) bool
	backpressure BackpressurePolicy
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

// matches applies the pattern + predicate filter. Called under the bus
// RLock.
func (s *memSub) matches(env Envelope) bool {
	if !s.pattern.Matches(env.Subject) {
		return false
	}
	if s.predicate != nil && !s.predicate(env) {
		return false
	}
	return true
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

	for _, sub := range b.subscribers {
		if !sub.matches(env) {
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
				return ctx.Err()
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
	}
	for _, fn := range opts {
		fn(&o)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBusClosed
	}

	sub := &memSub{
		id:           SubscriptionID(fmt.Sprintf("ms-%d", b.subSeq.Add(1))),
		pattern:      pattern,
		predicate:    o.predicate,
		backpressure: o.backpressure,
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
	delete(b.subscribers, id)
	b.mu.Unlock()
}

