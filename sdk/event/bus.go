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
	sampleRate   float64
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

// WithSampleRate configures the per-envelope keep probability when the
// subscription uses BackpressurePolicy Sample. Values are clamped to
// [0.0, 1.0]; values <= 0 reject every envelope (DropReasonSampled),
// values >= 1 pass every envelope through to the buffer-full check.
//
// Has no effect on subscriptions whose policy is not Sample, so it is
// safe to set unconditionally on a shared option set.
func WithSampleRate(rate float64) SubOption {
	return func(o *subOptions) {
		switch {
		case rate <= 0:
			o.sampleRate = 0
		case rate >= 1:
			o.sampleRate = 1
		default:
			o.sampleRate = rate
		}
	}
}

// AckSubscription is the optional capability a Subscription may expose
// when its parent Bus implements at-least-once delivery (typically
// cross-process buses backed by NATS / Redis Streams / Kafka). In-process
// implementations such as MemoryBus do not implement this interface;
// consumers should type-assert and treat the absence as "auto-ack".
//
// Semantics (v1, intentionally minimal):
//
//   - Ack confirms successful processing of the envelope identified by
//     envelopeID. Implementations MUST tolerate duplicate Ack calls and
//     return nil on the second invocation.
//   - Ack on an unknown envelopeID returns nil (defensive — replays and
//     restarts can legitimately surface envelopes outside the current
//     subscription's tracked window).
//   - Negative-acknowledge / explicit redelivery is intentionally NOT
//     part of v1. Implementations that need it should expose an
//     additional, named interface (e.g. NackSubscription) so callers can
//     opt in without breaking the minimal contract.
//
// Bus implementations without persistent delivery (MemoryBus,
// NoopBus) MUST NOT implement this interface; their consumers therefore
// take the auto-ack branch automatically.
type AckSubscription interface {
	Subscription

	// Ack acknowledges that envelopeID has been successfully processed.
	// See the AckSubscription doc comment for the per-call contract.
	Ack(envelopeID string) error
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
