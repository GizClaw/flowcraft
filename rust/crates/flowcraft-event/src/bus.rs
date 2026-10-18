//! The in-process bus: core's `MemoryBus`, on tokio.
//!
//! # What changed on the way over, and why
//!
//! core's `MemoryBus` is a Go channel per subscription plus four locks and two
//! wait groups. Most of that machinery exists to keep `close(ch)` from racing
//! a `ch <- env` -- the "send on closed channel" panic. The port does not have
//! that problem, because a subscription's buffer is a `VecDeque` behind a
//! mutex, not a channel: there is no object to close under a sender's feet.
//! What remains is the observable contract, and it is what this file keeps:
//!
//! - a publish fills in `id` and `time` when the producer left them empty;
//! - on return, every matching subscription has the envelope enqueued or has
//!   dropped it for a reported reason -- except under [`Backpressure::Block`],
//!   where the future is pending until there is room (core parks the
//!   goroutine; here cancelling is dropping the future, so a deadline is
//!   `tokio::time::timeout` rather than a context);
//! - policies are per subscription and independent: one full buffer never
//!   affects another subscription;
//! - `close` is idempotent, wakes every parked publisher and reader, and
//!   leaves queued envelopes readable before the stream ends (core closes the
//!   channel with its buffer still in it);
//! - the observer sees one `on_publish`, then one `on_deliver` / `on_drop` per
//!   subscription, all outside any lock.
//!
//! Two things are deliberately not here: the rate-limited drop log and the
//! metrics counters that go with it (they need the telemetry crate, which the
//! port has not built), and the trace/span ids core copies off the context
//! (`Publish` has no context; the runtime slice owns tracing). [`MemoryBus::dropped`]
//! is the count everything else would be derived from.
//!
//! # The wake-up design
//!
//! One `Mutex<VecDeque>` per subscription, and one `watch` channel per
//! subscription carrying a version that every change bumps. Readers and parked
//! publishers wait on a *newer version*, which is a reliable signal: it is not
//! a stored permit that one waiter can swallow (the trap a bare `Notify` sets
//! when the terminal signal has to reach everybody), and it cannot be missed
//! in the window between "checked the queue" and "went to sleep", because the
//! waiter subscribes to the version *before* checking. Every waiter re-reads
//! the queue under the lock after waking, so a spurious wake costs a lock and
//! nothing else.
//!
//! The trade-off: a change wakes every waiter of that subscription rather than
//! one. With one reader and the publishers currently blocked on it, that is
//! the same set.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::error::Error as StdError;
use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, RwLock, RwLockReadGuard, RwLockWriteGuard};

use flowcraft_contract::error::{Error as ContractError, ErrorKind};
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use crate::envelope::Envelope;
use crate::id;
use crate::policy::{
    Backpressure, DropReason, MAX_BUFFER_SIZE, Observer, Predicate, SubscribeOptions,
    SubscriptionId, sampled,
};
use crate::subject::{Pattern, Subject};
use crate::time::Timestamp;

/// The bus's own refusals.
///
/// Three, because they are the three a caller can act on differently. core
/// spells the first as the sentinel `ErrBusClosed` and the others as wrapped
/// validation errors; the port spells them as variants so a caller matches
/// instead of `errors.Is`-ing down a chain.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// The bus is closed: this publish or subscribe arrived after `close`.
    Closed,
    /// The pattern did not parse; the message is [`Pattern::parse`]'s.
    InvalidPattern(String),
    /// `buffer_size` is above [`MAX_BUFFER_SIZE`].
    BufferTooLarge {
        /// What the caller asked for.
        size: usize,
        /// The limit.
        max: usize,
    },
}

impl Error {
    /// The classification a [`ContractError`] gets when this converts.
    ///
    /// core leaves `ErrBusClosed` unclassified; the port's shared error type
    /// cannot say "no kind", so the conversion picks one and says so here:
    /// a closed bus is 503 ([`ErrorKind::NotAvailable`]), a bad pattern or a
    /// silly buffer size is the caller's input ([`ErrorKind::Validation`]).
    pub fn kind(&self) -> ErrorKind {
        match self {
            Error::Closed => ErrorKind::NotAvailable,
            Error::InvalidPattern(_) | Error::BufferTooLarge { .. } => ErrorKind::Validation,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Closed => f.write_str("event: bus closed"),
            Error::InvalidPattern(message) => f.write_str(message),
            Error::BufferTooLarge { size, max } => write!(
                f,
                "event: subscription buffer size {size} exceeds max {max}"
            ),
        }
    }
}

impl StdError for Error {}

impl From<Error> for ContractError {
    fn from(error: Error) -> ContractError {
        let kind = error.kind();
        ContractError::with_source(kind, error.to_string(), Box::new(error))
    }
}

/// A publish-and-subscribe channel for envelopes routed by subject.
///
/// The boxed return of [`publish`](Bus::publish) is what lets `Arc<dyn Bus>`
/// exist at all -- an `async fn` in a trait is not dyn-compatible. A concrete
/// bus can be used without noticing: `bus.publish(env).await`.
pub trait Bus: Send + Sync {
    /// Deliver `envelope` to every matching subscription.
    ///
    /// Returns [`Error::Closed`] once [`close`](Bus::close) has been invoked.
    fn publish<'a>(
        &'a self,
        envelope: Envelope,
    ) -> Pin<Box<dyn Future<Output = Result<(), Error>> + Send + 'a>>;

    /// Create a subscription matching `pattern`.
    fn subscribe(&self, pattern: Pattern, options: SubscribeOptions)
    -> Result<Subscription, Error>;

    /// Attach an observer after construction -- the wiring phase. Refused once
    /// the bus is closed.
    fn attach_observer(&self, observer: Arc<dyn Observer>) -> Result<(), Error>;

    /// Shut down: every subscription is closed and later calls are refused.
    /// Idempotent.
    fn close(&self) -> Result<(), Error>;
}

/// How many subjects the route cache remembers: core's
/// `defaultRouteCacheSize`.
pub const DEFAULT_ROUTE_CACHE_SIZE: usize = 1024;

/// Construction options, core's `MemoryBusOption` set.
#[derive(Default)]
pub struct MemoryBusOptions {
    observer: Option<Arc<dyn Observer>>,
    route_cache_size: Option<usize>,
}

impl MemoryBusOptions {
    /// An empty set: no observer, default route cache.
    pub fn new() -> MemoryBusOptions {
        MemoryBusOptions::default()
    }

    /// Attach an observer for lifecycle instrumentation. Drops are counted
    /// either way ([`MemoryBus::dropped`]); an observer adds per-subscription
    /// detail.
    pub fn observer(mut self, observer: Arc<dyn Observer>) -> Self {
        self.observer = Some(observer);
        self
    }

    /// Bound the subject-to-subscribers route cache.
    ///
    /// `0` disables it, so every publish scans the subscriptions -- the right
    /// choice when subjects embed an id and are never repeated. core's "a
    /// negative value is ignored" has no equivalent: the parameter is
    /// unsigned, so the default simply applies.
    pub fn route_cache_size(mut self, size: usize) -> Self {
        self.route_cache_size = Some(size);
        self
    }
}

/// The in-process bus implementation.
///
/// Cheap to clone (one `Arc`), and a clone is the same bus.
#[derive(Clone)]
pub struct MemoryBus {
    inner: Arc<Inner>,
}

impl MemoryBus {
    /// Construct a bus. core's `NewMemoryBus`.
    pub fn new(options: MemoryBusOptions) -> MemoryBus {
        let route_cache_size = options.route_cache_size.unwrap_or(DEFAULT_ROUTE_CACHE_SIZE);
        MemoryBus {
            inner: Arc::new(Inner {
                subscribers: RwLock::new(BTreeMap::new()),
                route_cache: RwLock::new(HashMap::new()),
                route_cache_size,
                observer: RwLock::new(options.observer.unwrap_or_else(|| Arc::new(NoopObserver))),
                closed: AtomicBool::new(false),
                dropped: AtomicU64::new(0),
                sub_seq: AtomicU64::new(0),
                rng: AtomicU64::new(
                    Timestamp::now().nanos() as u64
                        ^ (Timestamp::now().secs() as u64) << 32
                        ^ 0x5DEE_CE66_D1CE_4E5B,
                ),
            }),
        }
    }

    /// The cumulative count of envelopes dropped by a backpressure policy.
    ///
    /// The same tally core exposes as `Dropped()`, and the number the drop
    /// metrics and the rate-limited log are derived from.
    pub fn dropped(&self) -> u64 {
        self.inner.dropped.load(Ordering::SeqCst)
    }

    /// Whether [`close`](Bus::close) has run.
    pub fn is_closed(&self) -> bool {
        self.inner.closed.load(Ordering::SeqCst)
    }
}

impl Bus for MemoryBus {
    fn publish<'a>(
        &'a self,
        envelope: Envelope,
    ) -> Pin<Box<dyn Future<Output = Result<(), Error>> + Send + 'a>> {
        Box::pin(self.publish_inner(envelope))
    }

    fn subscribe(
        &self,
        pattern: Pattern,
        options: SubscribeOptions,
    ) -> Result<Subscription, Error> {
        self.subscribe_inner(pattern, options)
    }

    fn attach_observer(&self, observer: Arc<dyn Observer>) -> Result<(), Error> {
        if self.inner.closed.load(Ordering::SeqCst) {
            return Err(Error::Closed);
        }
        *write(&self.inner.observer) = observer;
        Ok(())
    }

    fn close(&self) -> Result<(), Error> {
        if self.inner.closed.swap(true, Ordering::SeqCst) {
            return Ok(());
        }
        let subscribers = {
            let mut subscribers = write(&self.inner.subscribers);
            self.inner.invalidate_routes();
            std::mem::take(&mut *subscribers)
        };
        for subscription in subscribers.into_values() {
            subscription.close();
        }
        Ok(())
    }
}

impl MemoryBus {
    async fn publish_inner(&self, envelope: Envelope) -> Result<(), Error> {
        if self.inner.closed.load(Ordering::SeqCst) {
            return Err(Error::Closed);
        }

        let mut envelope = envelope;
        if envelope.id.is_empty() {
            envelope.id = id::next();
        }
        if envelope.time.is_zero() {
            envelope.time = Timestamp::now();
        }

        let matched = self.inner.route(&envelope.subject);
        let mut delivered: Vec<SubscriptionId> = Vec::new();
        let mut dropped: Vec<(SubscriptionId, DropReason)> = Vec::new();

        for subscription in matched {
            // A predicate may read the envelope, so it runs per publish; the
            // pattern is what gets memoised.
            if subscription
                .predicate
                .as_ref()
                .is_some_and(|predicate| !predicate(&envelope))
            {
                continue;
            }
            if subscription.is_closed() {
                self.inner
                    .record(&mut dropped, subscription.id(), DropReason::Closed);
                continue;
            }
            match subscription.backpressure {
                Backpressure::Block => {
                    let mut changes = subscription.changes.subscribe();
                    loop {
                        if self.inner.closed.load(Ordering::SeqCst) {
                            // core returns ErrBusClosed here, after reporting
                            // what did land.
                            self.inner
                                .record(&mut dropped, subscription.id(), DropReason::Closed);
                            self.inner.fire(&envelope, &delivered, &dropped);
                            return Err(Error::Closed);
                        }
                        if subscription.is_closed() {
                            self.inner
                                .record(&mut dropped, subscription.id(), DropReason::Closed);
                            break;
                        }
                        match subscription.offer(envelope.clone(), &self.inner.rng) {
                            Offer::Delivered => {
                                delivered.push(subscription.id().clone());
                                break;
                            }
                            Offer::DeliveredEvicted => {
                                self.inner.record(
                                    &mut dropped,
                                    subscription.id(),
                                    DropReason::BufferFull,
                                );
                                delivered.push(subscription.id().clone());
                                break;
                            }
                            Offer::Dropped(reason) => {
                                self.inner.record(&mut dropped, subscription.id(), reason);
                                break;
                            }
                            Offer::Full => {}
                        }
                        // A bump after this point is seen: the version was
                        // subscribed to before the queue was read.
                        let _ = changes.changed().await;
                    }
                }
                policy => match subscription.offer(envelope.clone(), &self.inner.rng) {
                    Offer::Delivered => delivered.push(subscription.id().clone()),
                    Offer::DeliveredEvicted => {
                        self.inner
                            .record(&mut dropped, subscription.id(), DropReason::BufferFull);
                        delivered.push(subscription.id().clone());
                    }
                    Offer::Dropped(reason) => {
                        self.inner.record(&mut dropped, subscription.id(), reason)
                    }
                    // Non-blocking policies never report a full buffer: they
                    // resolve it here, which is the difference.
                    Offer::Full => {
                        debug_assert!(false, "{policy} answered Full");
                    }
                },
            }
        }

        self.inner.fire(&envelope, &delivered, &dropped);
        Ok(())
    }

    fn subscribe_inner(
        &self,
        pattern: Pattern,
        options: SubscribeOptions,
    ) -> Result<Subscription, Error> {
        if options.buffer_size > MAX_BUFFER_SIZE {
            return Err(Error::BufferTooLarge {
                size: options.buffer_size,
                max: MAX_BUFFER_SIZE,
            });
        }
        if self.inner.closed.load(Ordering::SeqCst) {
            return Err(Error::Closed);
        }

        let id = SubscriptionId::new(format!(
            "ms-{}",
            self.inner.sub_seq.fetch_add(1, Ordering::SeqCst) + 1
        ));
        let (changes, _) = watch::channel(0u64);
        let inner = Arc::new(SubInner {
            id: id.clone(),
            pattern,
            predicate: options.predicate,
            backpressure: options.backpressure,
            sample_rate: options.sample_rate,
            capacity: options.buffer_size,
            queue: Mutex::new(VecDeque::with_capacity(options.buffer_size)),
            closed: AtomicBool::new(false),
            changes,
        });

        {
            let mut subscribers = write(&self.inner.subscribers);
            // Re-check under the lock: close() flips the flag before draining,
            // so either it sees us or we see it.
            if self.inner.closed.load(Ordering::SeqCst) {
                return Err(Error::Closed);
            }
            // Every subscription changes the pattern set, so every cached
            // subject-to-subscribers entry is stale. Taken while holding the
            // subscribers lock, which is the one lock order this file keeps
            // (subscribers, then route cache).
            self.inner.invalidate_routes();
            subscribers.insert(id, inner.clone());
        }

        let subscription = Subscription {
            bus: self.inner.clone(),
            inner: inner.clone(),
            cancel: options.cancel.clone(),
        };
        if let Some(token) = options.cancel {
            // core spawns a watcher goroutine per subscription for this; so do
            // we, but only when there is a runtime to spawn on -- and it exits
            // on either signal, so a closed subscription does not leave it
            // parked.
            if let Ok(handle) = tokio::runtime::Handle::try_current() {
                let bus = self.inner.clone();
                let mut changes = inner.changes.subscribe();
                handle.spawn(async move {
                    loop {
                        if inner.is_closed() {
                            return;
                        }
                        tokio::select! {
                            biased;
                            _ = token.cancelled() => {
                                close_subscription(&bus, &inner);
                                return;
                            }
                            _ = changes.changed() => {}
                        }
                    }
                });
            }
        }
        Ok(subscription)
    }
}

/// One subscriber, as the bus sees it.
struct SubInner {
    id: SubscriptionId,
    pattern: Pattern,
    predicate: Option<Predicate>,
    backpressure: Backpressure,
    sample_rate: f64,
    capacity: usize,
    queue: Mutex<VecDeque<Envelope>>,
    closed: AtomicBool,
    /// Version channel: bumped on every change to `queue` or `closed`.
    changes: watch::Sender<u64>,
}

/// What a subscription did with an envelope.
enum Offer {
    /// Enqueued.
    Delivered,
    /// Enqueued, and the oldest buffered envelope was evicted for it.
    DeliveredEvicted,
    /// Refused, for this reason.
    Dropped(DropReason),
    /// No room, and the policy is willing to wait.
    Full,
}

impl SubInner {
    fn id(&self) -> &SubscriptionId {
        &self.id
    }

    fn is_closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    fn close(&self) {
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        self.bump();
    }

    fn bump(&self) {
        self.changes
            .send_modify(|version| *version = version.wrapping_add(1));
    }

    /// Take the front envelope, if any. Frees no capacity accounting of its
    /// own: the queue length *is* the accounting.
    fn try_pop(&self) -> Option<Envelope> {
        let mut queue = lock(&self.queue);
        let envelope = queue.pop_front();
        if envelope.is_some() {
            self.bump();
        }
        envelope
    }

    /// Apply the subscription's policy to one envelope.
    fn offer(&self, envelope: Envelope, rng: &AtomicU64) -> Offer {
        let mut queue = lock(&self.queue);
        let full = queue.len() >= self.capacity;
        match self.backpressure {
            Backpressure::DropNewest => {
                if full {
                    return Offer::Dropped(DropReason::BufferFull);
                }
                queue.push_back(envelope);
                drop(queue);
                self.bump();
                Offer::Delivered
            }
            Backpressure::DropOldest => {
                let evicted = if full {
                    queue.pop_front().is_some()
                } else {
                    false
                };
                queue.push_back(envelope);
                drop(queue);
                self.bump();
                if evicted {
                    Offer::DeliveredEvicted
                } else {
                    Offer::Delivered
                }
            }
            Backpressure::Sample => {
                let mut state = rng.load(Ordering::Relaxed);
                let keep = sampled(self.sample_rate, &mut state);
                rng.store(state, Ordering::Relaxed);
                if !keep {
                    return Offer::Dropped(DropReason::Sampled);
                }
                if full {
                    return Offer::Dropped(DropReason::BufferFull);
                }
                queue.push_back(envelope);
                drop(queue);
                self.bump();
                Offer::Delivered
            }
            Backpressure::Block => {
                if full {
                    return Offer::Full;
                }
                queue.push_back(envelope);
                drop(queue);
                self.bump();
                Offer::Delivered
            }
        }
    }
}

/// The bus's shared state. One per bus, shared by every handle.
struct Inner {
    /// Guarded first; the route cache is only ever taken while this one is
    /// held (or not held at all), never the other way round.
    subscribers: RwLock<BTreeMap<SubscriptionId, Arc<SubInner>>>,
    route_cache: RwLock<HashMap<Subject, Vec<Arc<SubInner>>>>,
    /// Zero disables the cache.
    route_cache_size: usize,
    observer: RwLock<Arc<dyn Observer>>,
    closed: AtomicBool,
    dropped: AtomicU64,
    sub_seq: AtomicU64,
    rng: AtomicU64,
}

impl Inner {
    fn route(&self, subject: &Subject) -> Vec<Arc<SubInner>> {
        if self.route_cache_size > 0
            && let Some(hit) = read(&self.route_cache).get(subject)
        {
            return hit.clone();
        }
        let matched: Vec<Arc<SubInner>> = {
            let subscribers = read(&self.subscribers);
            subscribers
                .values()
                .filter(|subscription| subscription.pattern.matches(subject))
                .cloned()
                .collect()
        };
        if self.route_cache_size > 0 {
            let mut cache = write(&self.route_cache);
            if cache.len() >= self.route_cache_size {
                // core's poor-man's LRU: wholesale clear on overflow, which is
                // adequate while the working set is small or churns slowly.
                cache.clear();
            }
            cache.insert(subject.clone(), matched.clone());
        }
        matched
    }

    fn invalidate_routes(&self) {
        if self.route_cache_size > 0 {
            write(&self.route_cache).clear();
        }
    }

    fn record(
        &self,
        dropped: &mut Vec<(SubscriptionId, DropReason)>,
        subscription: &SubscriptionId,
        reason: DropReason,
    ) {
        // core does not count a Closed drop in the tally: the subscription
        // went away on purpose, nothing was lost to a full buffer.
        if reason != DropReason::Closed {
            self.dropped.fetch_add(1, Ordering::SeqCst);
        }
        dropped.push((subscription.clone(), reason));
    }

    fn fire(
        &self,
        envelope: &Envelope,
        delivered: &[SubscriptionId],
        dropped: &[(SubscriptionId, DropReason)],
    ) {
        // The observer handle is taken and the lock released before any
        // callback: an observer may take its own locks, and must not be able
        // to block a subscriber scan.
        let observer = read(&self.observer).clone();
        observer.on_publish(envelope);
        for subscription in delivered {
            observer.on_deliver(subscription, envelope);
        }
        for (subscription, reason) in dropped {
            observer.on_drop(subscription, envelope, *reason);
        }
    }
}

fn close_subscription(bus: &Inner, subscription: &Arc<SubInner>) {
    if !bus.closed.load(Ordering::SeqCst) {
        let removed = {
            let mut subscribers = write(&bus.subscribers);
            let removed = subscribers.remove(subscription.id()).is_some();
            if removed {
                bus.invalidate_routes();
            }
            removed
        };
        let _ = removed;
    }
    subscription.close();
}

/// An active receive-side handle.
///
/// Dropping it closes it: a subscription nobody holds is a buffer nobody will
/// read, and core's equivalent (a channel with no reader) is the same silence.
#[must_use = "a subscription closes when dropped"]
pub struct Subscription {
    bus: Arc<Inner>,
    inner: Arc<SubInner>,
    cancel: Option<CancellationToken>,
}

impl Subscription {
    /// Uniquely identifies this subscription within its bus.
    pub fn id(&self) -> &SubscriptionId {
        self.inner.id()
    }

    /// The pattern it was created with.
    pub fn pattern(&self) -> &Pattern {
        &self.inner.pattern
    }

    /// What the buffer holds right now. Diagnostics only: by the time a caller
    /// looks, a publisher may have changed it.
    pub fn buffered(&self) -> usize {
        lock(&self.inner.queue).len()
    }

    /// Take a queued envelope without waiting, if one is there.
    pub fn try_recv(&self) -> Option<Envelope> {
        self.inner.try_pop()
    }

    /// Wait for the next envelope; `None` once the subscription (or its bus)
    /// is closed and the buffer is drained.
    ///
    /// Cancel-safe: dropping the future loses no envelope, because nothing is
    /// removed from the buffer until there is one to return synchronously.
    pub async fn recv(&mut self) -> Option<Envelope> {
        loop {
            if let Some(envelope) = self.inner.try_pop() {
                return Some(envelope);
            }
            if self.inner.is_closed() {
                return None;
            }
            // Subscribe to the version *before* reading the queue, so a change
            // between the read and the sleep is not missed.
            let mut changes = self.inner.changes.subscribe();
            if let Some(envelope) = self.inner.try_pop() {
                return Some(envelope);
            }
            if self.inner.is_closed() {
                return None;
            }
            match &self.cancel {
                Some(token) => {
                    tokio::select! {
                        biased;
                        _ = token.cancelled() => self.close(),
                        _ = changes.changed() => {}
                    }
                }
                None => {
                    let _ = changes.changed().await;
                }
            }
        }
    }

    /// Close the subscription. Idempotent; queued envelopes stay readable.
    pub fn close(&self) {
        close_subscription(&self.bus, &self.inner);
    }
}

impl fmt::Debug for Subscription {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Subscription")
            .field("id", self.inner.id())
            .field("pattern", &self.inner.pattern.as_str())
            .field("backpressure", &self.inner.backpressure)
            .finish_non_exhaustive()
    }
}

impl Drop for Subscription {
    fn drop(&mut self) {
        self.close();
    }
}

/// The observer a bus uses until one is attached.
struct NoopObserver;

impl Observer for NoopObserver {}

/// A poisoned lock is not a reason to fail a publish: core's mutexes do not
/// poison, and one panicking observer must not take the bus down for everyone
/// else. The state behind the lock is still consistent -- it is written under
/// the same lock -- so the poison flag is only information.
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

fn read<T>(lock: &RwLock<T>) -> RwLockReadGuard<'_, T> {
    lock.read().unwrap_or_else(|poisoned| poisoned.into_inner())
}

fn write<T>(lock: &RwLock<T>) -> RwLockWriteGuard<'_, T> {
    lock.write()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}
