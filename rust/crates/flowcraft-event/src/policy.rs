//! What a bus does with an envelope a subscription cannot take, and who
//! hears about it.
//!
//! core keeps these in `observer.go` because they are what an observer
//! reports. The port keeps them apart from the bus for the same reason: the
//! policies are a vocabulary both an in-process bus and a future cross-process
//! one have to speak.

use std::fmt;

use tokio_util::sync::CancellationToken;

use crate::envelope::Envelope;

/// A subscription's identity within one bus instance.
///
/// core formats these as `ms-<n>`; the port keeps the shape so a log line from
/// either side says the same thing. Opaque otherwise.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct SubscriptionId(String);

impl SubscriptionId {
    /// Wraps a bus-chosen identifier. Not validated: the bus owns the format.
    pub fn new(id: impl Into<String>) -> SubscriptionId {
        SubscriptionId(id.into())
    }

    /// The identifier itself.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for SubscriptionId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// What `publish` does when a subscription's buffer is full.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Backpressure {
    /// Drop the arriving envelope, keep what is buffered. The default:
    /// older events have presumably already been observed.
    #[default]
    DropNewest,
    /// Evict the oldest buffered envelope to make room. For state-like
    /// streams where the latest value is the valuable one.
    DropOldest,
    /// Wait until there is room.
    ///
    /// In core this parks the publishing goroutine until the context is
    /// cancelled; here it suspends the returned future, so cancelling is
    /// dropping it, and a slow subscriber back-pressures every publisher in
    /// the same way.
    Block,
    /// Keep each envelope with the subscription's [`sample_rate`] before the
    /// buffer is even consulted, so the delivered fraction is a property of
    /// the subscription and not of the subscriber's speed.
    ///
    /// [`sample_rate`]: SubscribeOptions::sample_rate
    Sample,
}

impl Backpressure {
    /// core's `String`: a stable label for diagnostics.
    pub fn as_str(self) -> &'static str {
        match self {
            Backpressure::DropNewest => "drop_newest",
            Backpressure::DropOldest => "drop_oldest",
            Backpressure::Block => "block",
            Backpressure::Sample => "sample",
        }
    }
}

impl fmt::Display for Backpressure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Why an envelope was discarded for one subscription.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DropReason {
    /// The buffer was full and the policy chose to drop.
    BufferFull,
    /// The publish raced a close, or the subscription was already closed.
    Closed,
    /// The [`Backpressure::Sample`] gate rejected it -- distinct from
    /// [`DropReason::BufferFull`] so a dashboard can tell "we chose to throw
    /// it away" from "the consumer could not keep up".
    Sampled,
}

impl DropReason {
    /// core's `String`: a stable label for diagnostics.
    pub fn as_str(self) -> &'static str {
        match self {
            DropReason::BufferFull => "buffer_full",
            DropReason::Closed => "closed",
            DropReason::Sampled => "sampled",
        }
    }
}

impl fmt::Display for DropReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The bus-level instrumentation hook: one place for metrics, tracing and drop
/// accounting.
///
/// Contract, inherited from core and enforced by how the bus calls you:
///
/// - every method runs outside any bus lock, so calling back into the bus
///   (`publish`, `subscribe`, `close`) does not deadlock -- it is still a bad
///   idea, because the bus is mid-publish and your call's own deliveries would
///   interleave with the ones it is reporting;
/// - every method runs on the publishing task and must return promptly;
/// - methods may run concurrently from several publishers;
/// - for one `publish`, [`on_publish`] runs exactly once, before any delivery
///   decision is reported, then zero or more [`on_deliver`] / [`on_drop`]
///   calls, one per matching subscription. Their relative order is the bus's
///   internal scan order and is not a contract.
///
/// All methods have empty defaults so an observer implements only what it
/// needs.
///
/// [`on_publish`]: Observer::on_publish
/// [`on_deliver`]: Observer::on_deliver
/// [`on_drop`]: Observer::on_drop
pub trait Observer: Send + Sync {
    /// One envelope was handed to the bus, before any delivery decision.
    fn on_publish(&self, _envelope: &Envelope) {}

    /// The envelope was enqueued for this subscription.
    fn on_deliver(&self, _subscription: &SubscriptionId, _envelope: &Envelope) {}

    /// The envelope was discarded for this subscription.
    ///
    /// For [`Backpressure::DropOldest`] this reports the *arriving* envelope
    /// with [`DropReason::BufferFull`] -- the same envelope core reports --
    /// while the envelope actually lost is the one evicted from the front of
    /// the buffer. One such call accompanies each eviction.
    fn on_drop(&self, _subscription: &SubscriptionId, _envelope: &Envelope, _reason: DropReason) {}
}

/// The filter a subscription applies after its pattern matched.
pub type Predicate = Box<dyn Fn(&Envelope) -> bool + Send + Sync>;

/// A buffer size omission: core's `WithBufferSize(0)` and below.
pub const DEFAULT_BUFFER_SIZE: usize = 64;

/// The largest buffer `subscribe` accepts: core's `maxMemoryBufferSize`.
pub const MAX_BUFFER_SIZE: usize = 1 << 20;

/// How a subscription receives.
///
/// core spells this `Subscribe(ctx, pattern, WithBufferSize(...), ...)`. The
/// port takes a struct because the options are read once and the closure is
/// not `Copy`; the builder methods are named after core's options.
pub struct SubscribeOptions {
    pub(crate) buffer_size: usize,
    pub(crate) backpressure: Backpressure,
    pub(crate) sample_rate: f64,
    pub(crate) predicate: Option<Predicate>,
    pub(crate) cancel: Option<CancellationToken>,
}

impl Default for SubscribeOptions {
    fn default() -> SubscribeOptions {
        SubscribeOptions {
            buffer_size: DEFAULT_BUFFER_SIZE,
            backpressure: Backpressure::default(),
            sample_rate: 1.0,
            predicate: None,
            cancel: None,
        }
    }
}

impl SubscribeOptions {
    /// core's `WithBufferSize`: the buffered capacity. Zero means the default;
    /// more than [`MAX_BUFFER_SIZE`] is refused by `subscribe` rather than
    /// silently clamped.
    pub fn buffer_size(mut self, size: usize) -> Self {
        self.buffer_size = if size == 0 { DEFAULT_BUFFER_SIZE } else { size };
        self
    }

    /// core's `WithBackpressure`.
    pub fn backpressure(mut self, policy: Backpressure) -> Self {
        self.backpressure = policy;
        self
    }

    /// The per-envelope keep probability for [`Backpressure::Sample`], clamped
    /// to `[0, 1]`: zero rejects everything, one passes everything on to the
    /// buffer check. Ignored by the other policies, so it is safe to set
    /// unconditionally.
    pub fn sample_rate(mut self, rate: f64) -> Self {
        // `clamp` returns NaN for NaN (both comparisons fail), and the gate
        // treats a rate that cannot be a probability as "reject".
        self.sample_rate = rate.clamp(0.0, 1.0);
        self
    }

    /// A secondary filter, applied after the pattern matched.
    ///
    /// Pattern matching is memoised per subject; a predicate may inspect the
    /// envelope, so it runs on every publish.
    pub fn predicate(
        mut self,
        predicate: impl Fn(&Envelope) -> bool + Send + Sync + 'static,
    ) -> Self {
        self.predicate = Some(Box::new(predicate));
        self
    }

    /// Close the subscription when `token` is cancelled -- core's cancelling
    /// the `Subscribe` context.
    ///
    /// With a runtime present the close happens as soon as the token fires; a
    /// bus subscribed from outside a runtime closes on the next `recv`, which
    /// is the only place it could be observed anyway.
    pub fn cancel(mut self, token: CancellationToken) -> Self {
        self.cancel = Some(token);
        self
    }
}

/// A whole-number generator for the [`Backpressure::Sample`] gate.
///
/// Not cryptographic and not reproducible across runs: the decision only has to
/// be independent per envelope, which a splitmix64 counter gives. Keeping it
/// here (rather than a dependency) keeps the gate's cost visible and its
/// quality honest.
pub(crate) fn splitmix64(state: &mut u64) -> u64 {
    *state = state.wrapping_add(0x9E37_79B9_7F4A_7C15);
    let mut z = *state;
    z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
    z ^ (z >> 31)
}

/// The keep probability as `[0, 1)`; `rate` is already clamped.
pub(crate) fn sampled(rate: f64, state: &mut u64) -> bool {
    if rate >= 1.0 {
        return true;
    }
    if rate <= 0.0 || !rate.is_finite() {
        return false;
    }
    // 53 bits: the width an f64 mantissa can hold exactly.
    let draw = splitmix64(state) >> 11;
    (draw as f64) / ((1u64 << 53) as f64) < rate
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn labels_match_cores_strings() {
        assert_eq!(Backpressure::DropNewest.as_str(), "drop_newest");
        assert_eq!(Backpressure::DropOldest.as_str(), "drop_oldest");
        assert_eq!(Backpressure::Block.as_str(), "block");
        assert_eq!(Backpressure::Sample.as_str(), "sample");
        assert_eq!(DropReason::BufferFull.as_str(), "buffer_full");
        assert_eq!(DropReason::Closed.as_str(), "closed");
        assert_eq!(DropReason::Sampled.as_str(), "sampled");
    }

    #[test]
    fn sample_rate_is_clamped() {
        assert_eq!(
            SubscribeOptions::default().sample_rate(0.0).sample_rate,
            0.0
        );
        assert_eq!(
            SubscribeOptions::default().sample_rate(-3.0).sample_rate,
            0.0
        );
        assert_eq!(
            SubscribeOptions::default().sample_rate(2.0).sample_rate,
            1.0
        );
        assert_eq!(
            SubscribeOptions::default().sample_rate(0.25).sample_rate,
            0.25
        );
    }

    #[test]
    fn the_gate_delivers_at_the_rate_it_says() {
        let mut state = 0x1234_5678_9ABC_DEF0u64;
        let draws = 20_000;
        for rate in [0.1, 0.5, 0.9] {
            let kept = (0..draws).filter(|_| sampled(rate, &mut state)).count();
            let got = kept as f64 / draws as f64;
            assert!(
                (got - rate).abs() < 0.02,
                "rate {rate}: kept {got:.3} over {draws} draws"
            );
        }
    }

    #[test]
    fn the_gate_is_exact_at_the_edges() {
        let mut state = 0;
        assert!((0..1000).all(|_| sampled(1.0, &mut state)));
        assert!((0..1000).all(|_| !sampled(0.0, &mut state)));
        assert!((0..1000).all(|_| !sampled(f64::NAN, &mut state)));
    }
}
