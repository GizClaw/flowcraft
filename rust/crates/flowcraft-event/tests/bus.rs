//! The in-process bus, against core's `memory_test.go`.
//!
//! Each test here has a Go counterpart with the same name in spirit; where the
//! port had to make a different call (cancellation is dropping a future,
//! `Block` suspends rather than parks a goroutine) the test says so.
//!
//! The two properties worth stating once, because every test leans on them:
//!
//! - a publish returns only after every matching subscription has the envelope
//!   or has dropped it, so a test may read the buffer straight after awaiting;
//! - the observer runs on the publishing task, so its counters are visible to
//!   the test right after the publish too.

use std::future::Future;
use std::pin::pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::task::{Context, Poll, Wake, Waker};
use std::time::Duration;

use flowcraft_event::bus::DEFAULT_ROUTE_CACHE_SIZE;
use flowcraft_event::{
    Backpressure, Bus, DEFAULT_BUFFER_SIZE, DropReason, Envelope, Error, MAX_BUFFER_SIZE,
    MemoryBus, MemoryBusOptions, Observer, Pattern, Subject, SubscribeOptions, SubscriptionId,
};
use tokio_util::sync::CancellationToken;

/// The house style for a test that expects success: the codebase's lints deny
/// `unwrap` and warn on `expect`, and a panic with the operation in it reads
/// better than either.
fn must<T, E: std::fmt::Display>(result: Result<T, E>, what: &str) -> T {
    match result {
        Ok(value) => value,
        Err(err) => panic!("{what}: {err}"),
    }
}

/// The same for a refusal a test expects.
fn must_err<T, E>(result: Result<T, E>, what: &str) -> E {
    match result {
        Ok(_) => panic!("{what}: expected a refusal"),
        Err(err) => err,
    }
}

/// The same for an option a test expects to be there.
fn must_some<T>(value: Option<T>, what: &str) -> T {
    match value {
        Some(value) => value,
        None => panic!("{what}: absent"),
    }
}

fn subject(text: &str) -> Subject {
    match Subject::parse(text) {
        Ok(subject) => subject,
        Err(err) => panic!("{text} is not a subject: {err}"),
    }
}

fn pattern(text: &str) -> Pattern {
    match Pattern::parse(text) {
        Ok(pattern) => pattern,
        Err(err) => panic!("{text} is not a pattern: {err}"),
    }
}

fn envelope(text: &str) -> Envelope {
    Envelope::new(subject(text))
}

fn sub(bus: &MemoryBus, text: &str, options: SubscribeOptions) -> flowcraft_event::Subscription {
    match bus.subscribe(pattern(text), options) {
        Ok(subscription) => subscription,
        Err(err) => panic!("subscribe {text}: {err}"),
    }
}

/// Takes everything buffered right now, without waiting.
fn drain(subscription: &flowcraft_event::Subscription) -> Vec<Envelope> {
    let mut taken = Vec::new();
    while let Some(envelope) = subscription.try_recv() {
        taken.push(envelope);
    }
    taken
}

#[derive(Default)]
struct Counting {
    published: AtomicU64,
    delivered: std::sync::Mutex<Vec<String>>,
    dropped: std::sync::Mutex<Vec<(String, DropReason)>>,
}

impl Counting {
    fn delivered(&self) -> Vec<String> {
        match self.delivered.lock() {
            Ok(delivered) => delivered.clone(),
            Err(poisoned) => poisoned.into_inner().clone(),
        }
    }

    fn drops(&self) -> Vec<(String, DropReason)> {
        match self.dropped.lock() {
            Ok(drops) => drops.clone(),
            Err(poisoned) => poisoned.into_inner().clone(),
        }
    }

    fn count(&self, reason: DropReason) -> usize {
        self.drops()
            .into_iter()
            .filter(|(_, got)| *got == reason)
            .count()
    }

    fn count_for(&self, subscription: &str, reason: DropReason) -> usize {
        self.drops()
            .into_iter()
            .filter(|(id, got)| id == subscription && *got == reason)
            .count()
    }

    fn delivered_for(&self, subscription: &str) -> usize {
        self.delivered()
            .into_iter()
            .filter(|id| id == subscription)
            .count()
    }
}

impl Observer for Counting {
    fn on_publish(&self, _envelope: &Envelope) {
        self.published.fetch_add(1, Ordering::SeqCst);
    }

    fn on_deliver(&self, subscription: &SubscriptionId, _envelope: &Envelope) {
        match self.delivered.lock() {
            Ok(mut delivered) => delivered.push(subscription.as_str().to_owned()),
            Err(poisoned) => poisoned.into_inner().push(subscription.as_str().to_owned()),
        }
    }

    fn on_drop(&self, subscription: &SubscriptionId, _envelope: &Envelope, reason: DropReason) {
        match self.dropped.lock() {
            Ok(mut drops) => drops.push((subscription.as_str().to_owned(), reason)),
            Err(poisoned) => poisoned
                .into_inner()
                .push((subscription.as_str().to_owned(), reason)),
        }
    }
}

fn bus_with(observer: Arc<Counting>) -> MemoryBus {
    MemoryBus::new(MemoryBusOptions::new().observer(observer))
}

#[tokio::test]
async fn pattern_routing_reaches_only_matching_subscriptions() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let everything = sub(&bus, ">", SubscribeOptions::default());
    let only_a = sub(&bus, "a.>", SubscribeOptions::default());
    let exact = sub(&bus, "a.b", SubscribeOptions::default());

    for text in ["a.b", "a.c", "z"] {
        must(bus.publish(envelope(text)).await, "publish");
    }

    assert_eq!(drain(&everything).len(), 3);
    assert_eq!(drain(&only_a).len(), 2);
    let got = drain(&exact);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].subject.as_str(), "a.b");
}

#[tokio::test]
async fn a_predicate_filters_after_the_pattern() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let tenant = sub(
        &bus,
        ">",
        SubscribeOptions::default().predicate(|envelope| envelope.tenant() == "acme"),
    );

    let mut mine = envelope("x.1");
    mine.set_header("tenant", "acme");
    let theirs = envelope("x.2");
    must(bus.publish(mine).await, "publish");
    must(bus.publish(theirs).await, "publish");

    let got = drain(&tenant);
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].subject.as_str(), "x.1");
}

#[tokio::test]
async fn drop_newest_keeps_the_oldest_and_counts_both_losses() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::DropNewest),
    );

    for text in ["x.1", "x.2", "x.3"] {
        must(bus.publish(envelope(text)).await, "publish");
    }

    let got = drain(&subscription);
    assert_eq!(got.len(), 1);
    assert_eq!(
        got[0].subject.as_str(),
        "x.1",
        "DropNewest keeps the oldest"
    );
    assert_eq!(bus.dropped(), 2);
    assert_eq!(observer.count(DropReason::BufferFull), 2);
    assert_eq!(observer.delivered().len(), 1);
}

#[tokio::test]
async fn drop_oldest_keeps_the_newest_and_reports_each_eviction() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::DropOldest),
    );

    for text in ["x.1", "x.2", "x.3"] {
        must(bus.publish(envelope(text)).await, "publish");
    }

    let got = drain(&subscription);
    assert_eq!(got.len(), 1);
    assert_eq!(
        got[0].subject.as_str(),
        "x.3",
        "DropOldest keeps the newest"
    );
    // x.2 evicts x.1 and x.3 evicts x.2: two losses, both reported, and both
    // deliveries reported as well -- the arriving envelope did land.
    assert_eq!(observer.count(DropReason::BufferFull), 2);
    assert_eq!(observer.delivered().len(), 3);
    assert_eq!(bus.dropped(), 2);
}

#[tokio::test]
async fn drop_oldest_does_not_report_an_eviction_when_there_was_room() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(4)
            .backpressure(Backpressure::DropOldest),
    );

    must(bus.publish(envelope("x.1")).await, "publish");
    must(bus.publish(envelope("x.2")).await, "publish");

    assert_eq!(drain(&subscription).len(), 2);
    assert_eq!(observer.count(DropReason::BufferFull), 0);
    assert_eq!(bus.dropped(), 0);
}

#[tokio::test]
async fn block_waits_for_room_and_then_delivers() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let mut subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::Block),
    );

    must(bus.publish(envelope("x.1")).await, "publish");

    // The parked publish owns its envelope: the future is the send, so it has
    // to stay alive for the envelope to arrive (dropping it is cancelling).
    let publisher = bus.clone();
    let mut blocked = tokio::spawn(async move { publisher.publish(envelope("x.2")).await });
    // Nothing may complete while the buffer is full.
    assert!(
        tokio::time::timeout(Duration::from_millis(50), &mut blocked)
            .await
            .is_err(),
        "publish should not complete while the buffer is full"
    );

    let first = must_some(subscription.recv().await, "x.1");
    assert_eq!(first.subject.as_str(), "x.1");
    let outcome = must(
        must(
            tokio::time::timeout(Duration::from_secs(1), blocked).await,
            "the parked publish must wake",
        ),
        "task",
    );
    assert_eq!(outcome, Ok(()));
    let second = must_some(subscription.recv().await, "x.2");
    assert_eq!(second.subject.as_str(), "x.2");
    assert_eq!(bus.dropped(), 0);
}

#[tokio::test]
async fn block_is_only_slow_for_the_subscription_that_is_full() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let mut slow = sub(
        &bus,
        "slow.>",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::Block),
    );
    let fast = sub(&bus, "fast.>", SubscribeOptions::default().buffer_size(8));

    must(bus.publish(envelope("slow.1")).await, "publish");

    // The fast subscription is not back-pressured by the slow one; only the
    // blocked publish waits, and it starts after the fast delivery is done.
    let blocked = bus.publish(envelope("slow.2"));
    let fast_publish = bus.publish(envelope("fast.1")).await;
    assert!(fast_publish.is_ok(), "the other subscription is unaffected");
    assert_eq!(drain(&fast).len(), 1);

    assert!(
        tokio::time::timeout(Duration::from_millis(50), blocked)
            .await
            .is_err()
    );
    must_some(slow.recv().await, "slow.1");
}

#[tokio::test]
async fn block_gives_up_when_the_bus_closes() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let mut subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::Block),
    );
    must(bus.publish(envelope("x.1")).await, "publish");

    let closer = bus.clone();
    let parked = tokio::spawn(async move { closer.publish(envelope("x.2")).await });
    tokio::time::sleep(Duration::from_millis(20)).await;
    must(bus.close(), "close");

    let outcome = must(
        must(
            tokio::time::timeout(Duration::from_secs(1), parked).await,
            "the parked publish must wake",
        ),
        "task",
    );
    assert_eq!(outcome, Err(Error::Closed));
    assert_eq!(observer.count(DropReason::Closed), 1);

    // The queued envelope is still readable; the stream ends after it.
    assert_eq!(
        subscription.recv().await.map(|env| env.subject),
        Some(subject("x.1"))
    );
    assert!(subscription.recv().await.is_none());
}

#[tokio::test]
async fn block_reports_closed_when_the_subscription_closes() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::Block),
    );
    must(bus.publish(envelope("x.1")).await, "publish");

    let publisher = bus.clone();
    let parked = tokio::spawn(async move { publisher.publish(envelope("x.2")).await });
    tokio::time::sleep(Duration::from_millis(20)).await;
    subscription.close();

    let outcome = must(
        must(
            tokio::time::timeout(Duration::from_secs(1), parked).await,
            "the parked publish must wake",
        ),
        "task",
    );
    // The bus is fine; the subscription went away, so the publish succeeded
    // having lost one envelope to a close.
    assert_eq!(outcome, Ok(()));
    assert_eq!(observer.count(DropReason::Closed), 1);
}

#[tokio::test]
async fn sample_rate_zero_drops_everything_as_sampled() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .backpressure(Backpressure::Sample)
            .sample_rate(0.0),
    );

    for index in 0..10 {
        must(
            bus.publish(envelope(&format!("x.{index}"))).await,
            "publish",
        );
    }

    assert_eq!(drain(&subscription).len(), 0);
    assert_eq!(observer.count(DropReason::Sampled), 10);
    assert_eq!(observer.count(DropReason::BufferFull), 0);
    assert_eq!(bus.dropped(), 10);
}

#[tokio::test]
async fn sample_rate_one_delivers_everything() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .backpressure(Backpressure::Sample)
            .sample_rate(1.0),
    );

    for index in 0..10 {
        must(
            bus.publish(envelope(&format!("x.{index}"))).await,
            "publish",
        );
    }

    assert_eq!(drain(&subscription).len(), 10);
    assert_eq!(bus.dropped(), 0);
}

#[tokio::test]
async fn sample_partial_rate_lands_within_its_band() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(MAX_BUFFER_SIZE)
            .backpressure(Backpressure::Sample)
            .sample_rate(0.25),
    );

    let published = 2_000;
    for index in 0..published {
        must(
            bus.publish(envelope(&format!("x.{index}"))).await,
            "publish",
        );
    }

    // The gate's job is a ratio independent of subscriber speed, so this is a
    // wide band on purpose: 25% of 2000 is 500, and three sigma of a binomial
    // is ~30.
    let kept = drain(&subscription).len();
    assert!(
        (430..=570).contains(&kept),
        "kept {kept} of {published} at rate 0.25"
    );
}

#[tokio::test]
async fn sample_is_gated_before_the_buffer_check() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(1)
            .backpressure(Backpressure::Sample)
            .sample_rate(0.5),
    );

    // Fill the buffer, then publish more: those are buffer-full drops (when
    // the gate lets them through) rather than sampled ones, which is the
    // distinction the two reasons exist for.
    must(bus.publish(envelope("x.1")).await, "publish");
    for index in 2..40 {
        must(
            bus.publish(envelope(&format!("x.{index}"))).await,
            "publish",
        );
    }

    let sampled = observer.count(DropReason::Sampled);
    let full = observer.count(DropReason::BufferFull);
    assert!(sampled > 0, "the gate rejected nothing out of 38 publishes");
    assert!(full > 0, "the gate passed nothing into a full buffer");
    assert_eq!(sampled + full + drain(&subscription).len(), 39);
}

#[tokio::test]
async fn publish_fills_the_id_and_the_time() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let subscription = sub(&bus, ">", SubscribeOptions::default());

    let mut bare = envelope("x.1");
    bare.id.clear();
    bare.time = flowcraft_event::time::Timestamp::ZERO;
    must(bus.publish(bare).await, "publish");

    let got = must_some(subscription.try_recv(), "one envelope");
    assert_eq!(got.id.len(), 20, "rs/xid's shape: {}", got.id);
    assert!(!got.time.is_zero());
}

#[tokio::test]
async fn a_publish_that_fits_does_not_yield() {
    // The promise of the non-blocking policies: a publish is a lock, a push and
    // an observer call, so a caller can make it from a hot loop without giving
    // the runtime a chance to run anything else.
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let _subscription = sub(&bus, ">", SubscribeOptions::default());

    let polled = poll_once(bus.publish(envelope("x.1")));
    assert_eq!(polled, Some(Ok::<(), Error>(())));
}

#[tokio::test]
async fn close_is_idempotent_and_refuses_later_calls() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let mut subscription = sub(&bus, ">", SubscribeOptions::default());

    assert_eq!(bus.close(), Ok(()));
    assert_eq!(bus.close(), Ok(()), "Close is idempotent");
    assert_eq!(bus.publish(envelope("x.1")).await, Err(Error::Closed));
    assert_eq!(
        bus.subscribe(pattern(">"), SubscribeOptions::default())
            .err(),
        Some(Error::Closed)
    );
    assert_eq!(
        bus.attach_observer(Arc::new(Counting::default())).err(),
        Some(Error::Closed)
    );
    assert!(subscription.recv().await.is_none());
}

#[tokio::test]
async fn close_leaves_queued_envelopes_readable() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let mut subscription = sub(&bus, ">", SubscribeOptions::default());
    must(bus.publish(envelope("x.1")).await, "publish");
    must(bus.publish(envelope("x.2")).await, "publish");

    must(bus.close(), "close");

    // core closes the channel with its buffer intact; so does this.
    assert_eq!(
        subscription.recv().await.map(|env| env.subject),
        Some(subject("x.1"))
    );
    assert_eq!(
        subscription.recv().await.map(|env| env.subject),
        Some(subject("x.2"))
    );
    assert!(subscription.recv().await.is_none());
}

#[tokio::test]
async fn a_buffer_larger_than_the_limit_is_refused() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let error = must_err(
        bus.subscribe(
            pattern(">"),
            SubscribeOptions::default().buffer_size(MAX_BUFFER_SIZE + 1),
        ),
        "over the limit",
    );
    assert_eq!(
        error,
        Error::BufferTooLarge {
            size: MAX_BUFFER_SIZE + 1,
            max: MAX_BUFFER_SIZE
        }
    );
    assert_eq!(
        error.to_string(),
        "event: subscription buffer size 1048577 exceeds max 1048576"
    );
}

#[tokio::test]
async fn a_zero_buffer_size_means_the_default() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let subscription = sub(&bus, ">", SubscribeOptions::default().buffer_size(0));
    // The default is 64: the 65th publish to an unread subscription drops.
    for index in 0..65 {
        must(
            bus.publish(envelope(&format!("x.{index}"))).await,
            "publish",
        );
    }
    assert_eq!(drain(&subscription).len(), DEFAULT_BUFFER_SIZE);
    assert_eq!(bus.dropped(), 1);
}

#[tokio::test]
async fn an_invalid_pattern_is_refused_at_parse_time() {
    // core validates in Subscribe and returns ErrInvalidPattern. The port
    // cannot be handed a bad pattern at all -- `Pattern` is the validation --
    // so the sentence moves to the parse, where it is still core's.
    let error = must_err(Pattern::parse("a.>.b"), "a tail wildcard in the middle");
    assert!(error.starts_with("event: invalid pattern: "), "{error}");
}

#[tokio::test]
async fn cancelling_the_token_closes_the_subscription() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let token = CancellationToken::new();
    let mut subscription = sub(&bus, ">", SubscribeOptions::default().cancel(token.clone()));

    token.cancel();
    assert!(
        must(
            tokio::time::timeout(Duration::from_secs(1), subscription.recv()).await,
            "the token must end the stream",
        )
        .is_none()
    );
    // And the bus no longer routes to it.
    must(bus.publish(envelope("x.1")).await, "publish");
    assert_eq!(drain(&subscription).len(), 0);
}

#[tokio::test]
async fn dropping_a_subscription_closes_it() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    {
        let _subscription = sub(&bus, ">", SubscribeOptions::default());
    }
    must(bus.publish(envelope("x.1")).await, "publish");
    assert_eq!(observer.delivered().len(), 0);
}

#[tokio::test]
async fn the_route_cache_notices_a_new_subscription() {
    let bus = MemoryBus::new(MemoryBusOptions::new());
    let first = sub(&bus, "a.>", SubscribeOptions::default());
    must(bus.publish(envelope("a.1")).await, "publish");
    assert_eq!(drain(&first).len(), 1);

    // The subject is cached now; a subscription created afterwards must still
    // be routed to, which means the cache was invalidated on subscribe.
    let second = sub(&bus, "a.*", SubscribeOptions::default());
    must(bus.publish(envelope("a.2")).await, "publish");
    assert_eq!(drain(&first).len(), 1);
    assert_eq!(drain(&second).len(), 1);
}

#[tokio::test]
async fn the_route_cache_can_be_switched_off() {
    let bus = MemoryBus::new(MemoryBusOptions::new().route_cache_size(0));
    assert_eq!(DEFAULT_ROUTE_CACHE_SIZE, 1024);
    let first = sub(&bus, "a.>", SubscribeOptions::default());
    let second = sub(&bus, "a.*", SubscribeOptions::default());
    must(bus.publish(envelope("a.1")).await, "publish");
    assert_eq!(drain(&first).len(), 1);
    assert_eq!(drain(&second).len(), 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn many_publishers_and_a_close_lose_nothing_unreported() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let _slow = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(4)
            .backpressure(Backpressure::DropOldest),
    );
    let _sampled = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(4)
            .backpressure(Backpressure::Sample)
            .sample_rate(0.5),
    );

    let mut publishers = Vec::new();
    for worker in 0..6 {
        let bus = bus.clone();
        publishers.push(tokio::spawn(async move {
            for index in 0..200 {
                let sent = bus.publish(envelope(&format!("w{worker}.{index}"))).await;
                if sent.is_err() {
                    // Once closed, every later publish is refused; that is the
                    // contract, not a failure.
                    break;
                }
            }
        }));
    }
    for publisher in publishers {
        must(
            must(
                tokio::time::timeout(Duration::from_secs(5), publisher).await,
                "publishers must finish",
            ),
            "task",
        );
    }
    must(bus.close(), "close");

    // Every publish that returned Ok is accounted for, per subscription. The
    // one that never reads its buffer is the interesting one: what it lost is
    // exactly what it was sent, minus what is still in the buffer.
    let published = observer.published.load(Ordering::SeqCst) as usize;
    assert_eq!(published, 1_200, "every publish before the close landed");

    let slow = _slow.id().as_str();
    let delivered = observer.delivered_for(slow);
    let evicted = observer.count_for(slow, DropReason::BufferFull);
    assert_eq!(delivered, published, "DropOldest delivers every envelope");
    assert_eq!(
        evicted,
        published - _slow.buffered(),
        "every eviction is reported, and nothing else is"
    );
    assert_eq!(
        observer.count_for(slow, DropReason::Sampled)
            + observer.count_for(slow, DropReason::Closed),
        0,
        "this policy has no other way to lose an envelope"
    );

    let sampled = _sampled.id().as_str();
    let kept = observer.delivered_for(sampled);
    let lost = observer.count_for(sampled, DropReason::Sampled)
        + observer.count_for(sampled, DropReason::BufferFull);
    assert_eq!(
        kept + lost,
        published,
        "the sampled subscription accounts for every envelope"
    );

    // And the tally the bus exposes is the sum of the reasons it counted.
    assert_eq!(
        bus.dropped() as usize,
        observer.count(DropReason::BufferFull) + observer.count(DropReason::Sampled)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn closing_under_pressure_terminates_and_refuses_the_rest() {
    let observer = Arc::new(Counting::default());
    let bus = bus_with(observer.clone());
    let mut subscription = sub(
        &bus,
        ">",
        SubscribeOptions::default()
            .buffer_size(2)
            .backpressure(Backpressure::Block),
    );

    // A Block subscriber with a full buffer parks its publishers, which is the
    // case core needs three phases and two wait groups to shut down. Here it
    // has to come back with Closed, and every publisher has to notice.
    let mut publishers = Vec::new();
    for worker in 0..4 {
        let bus = bus.clone();
        publishers.push(tokio::spawn(async move {
            let mut refused = 0u64;
            for index in 0..64 {
                if bus
                    .publish(envelope(&format!("w{worker}.{index}")))
                    .await
                    .is_err()
                {
                    refused += 1;
                }
            }
            refused
        }));
    }
    tokio::time::sleep(Duration::from_millis(5)).await;
    must(bus.close(), "close");

    let mut refused = 0;
    for publisher in publishers {
        refused += must(
            must(
                tokio::time::timeout(Duration::from_secs(5), publisher).await,
                "a parked publisher must be woken by close",
            ),
            "task",
        );
    }
    assert!(refused > 0, "the close must have refused something");
    assert_eq!(bus.publish(envelope("x.1")).await, Err(Error::Closed));

    // Whatever was delivered before the close is still readable, and the stream
    // ends.
    let mut drained = 0;
    while must(
        tokio::time::timeout(Duration::from_secs(1), subscription.recv()).await,
        "recv must not park after close",
    )
    .is_some()
    {
        drained += 1;
    }
    assert!(drained >= 2, "the buffered envelopes stay readable");
    assert_eq!(
        bus.dropped() as usize,
        observer.count(DropReason::BufferFull) + observer.count(DropReason::Sampled)
    );
}

/// Polls `future` once with a no-op waker: `Some(output)` when it completed
/// without being woken, `None` when it is pending.
fn poll_once<F: Future>(future: F) -> Option<F::Output> {
    struct Noop;
    impl Wake for Noop {
        fn wake(self: Arc<Self>) {}
    }
    let waker = Waker::from(Arc::new(Noop));
    let mut context = Context::from_waker(&waker);
    let mut future = pin!(future);
    match future.as_mut().poll(&mut context) {
        Poll::Ready(output) => Some(output),
        Poll::Pending => None,
    }
}
