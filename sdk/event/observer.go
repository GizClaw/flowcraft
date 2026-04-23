package event

// SubscriptionID identifies a subscription within a single Bus instance.
// It is opaque to consumers; bus implementations choose the format.
type SubscriptionID string

// BackpressurePolicy controls what happens when a subscription's buffer is
// already full at Publish time.
type BackpressurePolicy int

const (
	// DropNewest drops the incoming envelope and keeps existing buffered
	// items. Default policy: prioritises older, presumably already-observed
	// events.
	DropNewest BackpressurePolicy = iota

	// DropOldest drops the oldest buffered envelope to make room for the
	// new one. Use when the latest state is more valuable than history.
	DropOldest

	// Block makes Publish wait until either the buffer has room or the
	// publishing context is cancelled. Use sparingly: a slow subscriber
	// will back-pressure every publisher.
	Block
)

// String returns a stable label for diagnostics.
func (p BackpressurePolicy) String() string {
	switch p {
	case DropNewest:
		return "drop_newest"
	case DropOldest:
		return "drop_oldest"
	case Block:
		return "block"
	default:
		return "unknown"
	}
}

// DropReason explains why an envelope was discarded for a particular
// subscription.
type DropReason int

const (
	// DropReasonBufferFull indicates the subscription buffer was full and
	// the active BackpressurePolicy chose to drop.
	DropReasonBufferFull DropReason = iota

	// DropReasonClosed indicates Publish raced with subscription close;
	// the envelope was not delivered.
	DropReasonClosed
)

// String returns a stable label for diagnostics.
func (r DropReason) String() string {
	switch r {
	case DropReasonBufferFull:
		return "buffer_full"
	case DropReasonClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Observer is the bus-level instrumentation hook. It carries the
// OnPublish / OnDeliver / OnDrop lifecycle for every envelope, giving
// callers a single place to wire metrics / tracing / drop accounting.
//
// Concurrency contract — every Bus implementation must guarantee:
//
//   - Observer methods are invoked outside any bus-internal lock.
//   - Implementations must return promptly. They MUST NOT call back into
//     Bus methods (Publish / Subscribe / Close); doing so risks deadlock
//     and the bus is allowed to detect such calls and panic in debug
//     builds.
//   - Observer methods may be invoked concurrently from multiple
//     goroutines; implementations must be safe for concurrent use.
//
// Ordering:
//
//   - For a single Publish call, OnPublish is invoked exactly once,
//     followed by zero or more OnDeliver / OnDrop calls (one per
//     matching subscription).
//   - The relative order of OnDeliver / OnDrop calls within one Publish
//     reflects the bus's internal subscription scan and is therefore
//     implementation-defined. MemoryBus, for example, memoises the
//     match result per Subject so a hot subject sees a stable callback
//     order across publishes — observers MUST NOT rely on that order
//     being either stable or random.
//
// Observer is intentionally an interface (not a func) so a single hook
// can correlate the three lifecycle moments without ad-hoc closures.
type Observer interface {
	// OnPublish fires once per Publish call, before any delivery decision.
	OnPublish(env Envelope)
	// OnDeliver fires after a successful enqueue into a subscription's
	// channel.
	OnDeliver(subID SubscriptionID, env Envelope)
	// OnDrop fires when an envelope is discarded for a specific
	// subscription, identifying the reason.
	OnDrop(subID SubscriptionID, env Envelope, reason DropReason)
}

// noopObserver is used when no observer is configured to keep hot-path
// branches uniform.
type noopObserver struct{}

func (noopObserver) OnPublish(Envelope)                          {}
func (noopObserver) OnDeliver(SubscriptionID, Envelope)          {}
func (noopObserver) OnDrop(SubscriptionID, Envelope, DropReason) {}
