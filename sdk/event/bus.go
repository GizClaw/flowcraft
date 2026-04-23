// Package event provides a subject-routed publish/subscribe bus for
// Envelopes, plus an in-memory implementation suitable for single-process
// fan-out.
//
// File layout:
//
//	bus.go         — Bus / Subscription / SubOption interface contract
//	                 plus NoopBus. This is the stable surface; everything
//	                 else in the package implements or supports it.
//	memory.go      — MemoryBus, the in-process Bus implementation.
//	envelope.go    — Envelope value type and well-known headers.
//	subject.go     — Subject / Pattern routing keys.
//	observer.go    — Observer instrumentation hook + BackpressurePolicy.
//	deprecated.go  — All v0.1.x compatibility shims (Legacy* types,
//	                 Event/EventType/EventFilter, LegacyMemoryBus).
//	                 Scheduled to be deleted in v0.2.0 in a single
//	                 commit; per-symbol Migration godoc gives the
//	                 new-API equivalent.
package event

import (
	"context"
	"errors"
)

// ErrBusClosed is returned by Publish / Subscribe after a Bus has been
// closed. Implementations must return this exact value (not a wrapped
// variant) so callers can compare with errors.Is.
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
