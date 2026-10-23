//! Capturing records in tests.
//!
//! Asserting on log output is how a convention stays a convention: a field that
//! is renamed, or an error that stops carrying its kind, fails a test instead
//! of quietly shipping.
//!
//! The capture is installed as a *thread-local* default subscriber, not a
//! global one, so tests in one binary can run in parallel and none of them can
//! steal another's records.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use tracing::field::{Field, Visit};
use tracing::{Event, Level, Subscriber};
use tracing_subscriber::Layer;
use tracing_subscriber::layer::SubscriberExt;

/// A span layer backed by an SDK provider with no exporter.
///
/// A test that asserts on trace ids needs the same span machinery the daemon
/// runs, not a stand-in: ids only exist once a span has been materialised.
#[cfg(feature = "otel")]
pub fn trace_layer()
-> impl tracing_subscriber::Layer<tracing_subscriber::Registry> + Send + Sync + 'static {
    let provider = crate::otel::provider(&crate::Resource::default(), None);
    crate::otel::layer::<tracing_subscriber::Registry>(&provider)
}

/// One captured record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Captured {
    /// Severity.
    pub level: Level,
    /// The emitting module path.
    pub target: String,
    /// The formatted message.
    pub message: String,
    /// Every field, message excluded.
    pub fields: BTreeMap<String, String>,
}

impl Captured {
    /// The value of `key`, if the record carried it.
    #[must_use]
    pub fn field(&self, key: &str) -> Option<&str> {
        self.fields.get(key).map(String::as_str)
    }
}

/// A record collector.
///
/// Cloneable and shareable: the clone handed to the layer and the clone kept by
/// the test see the same buffer.
#[derive(Debug, Clone, Default)]
pub struct Capture {
    records: Arc<Mutex<Vec<Captured>>>,
}

impl Capture {
    /// An empty capture.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Run `body` with this capture installed as the thread-local default.
    pub fn with<R>(&self, body: impl FnOnce() -> R) -> R {
        let subscriber = tracing_subscriber::registry().with(self.clone());
        tracing::subscriber::with_default(subscriber, body)
    }

    /// Run `future` with this capture installed as the thread-local default.
    ///
    /// The guard is held across await points, so the returned future is not
    /// `Send`: use it on a single-threaded test runtime, which is also what
    /// keeps a spawned task on the captured thread.
    pub async fn with_async<F: std::future::Future>(&self, future: F) -> F::Output {
        let subscriber: tracing::Dispatch =
            tracing_subscriber::registry().with(self.clone()).into();
        let _guard = tracing::dispatcher::set_default(&subscriber);
        future.await
    }

    /// Every record captured so far.
    #[must_use]
    pub fn records(&self) -> Vec<Captured> {
        self.records
            .lock()
            .map(|guard| guard.clone())
            .unwrap_or_default()
    }

    /// The first record whose message is exactly `message`.
    #[must_use]
    pub fn find(&self, message: &str) -> Option<Captured> {
        self.records()
            .into_iter()
            .find(|record| record.message == message)
    }

    /// Whether a record at `level` with exactly `message` was captured.
    #[must_use]
    pub fn has(&self, level: Level, message: &str) -> bool {
        self.records()
            .into_iter()
            .any(|record| record.level == level && record.message == message)
    }

    /// Drop everything captured so far.
    pub fn clear(&self) {
        if let Ok(mut guard) = self.records.lock() {
            guard.clear();
        }
    }

    fn push(&self, record: Captured) {
        if let Ok(mut guard) = self.records.lock() {
            guard.push(record);
        }
    }
}

impl<S: Subscriber> Layer<S> for Capture {
    fn on_event(&self, event: &Event<'_>, _ctx: tracing_subscriber::layer::Context<'_, S>) {
        let mut visitor = Collect::default();
        event.record(&mut visitor);
        self.push(Captured {
            level: *event.metadata().level(),
            target: event.metadata().target().to_owned(),
            message: std::mem::take(&mut visitor.message),
            fields: visitor.fields,
        });
    }
}

#[derive(Default)]
struct Collect {
    message: String,
    fields: BTreeMap<String, String>,
}

impl Collect {
    fn push(&mut self, field: &Field, value: String) {
        if field.name() == "message" {
            self.message = value;
        } else {
            self.fields.insert(field.name().to_owned(), value);
        }
    }
}

impl Visit for Collect {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.push(field, format!("{value:?}"));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.push(field, value.to_owned());
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.push(field, value.to_string());
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.push(field, value.to_string());
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.push(field, value.to_string());
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        self.push(field, value.to_string());
    }

    fn record_error(&mut self, field: &Field, value: &(dyn std::error::Error + 'static)) {
        self.push(field, value.to_string());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::attrs::attr;

    #[test]
    fn captures_message_and_fields() {
        let capture = Capture::new();
        capture.with(|| {
            tracing::warn!(
                flowcraft.craft = "alpha",
                flowcraft.gen = 7,
                "craft stopped"
            );
        });
        let record = capture.find("craft stopped").expect("record");
        assert_eq!(record.level, Level::WARN);
        assert_eq!(record.field("flowcraft.craft"), Some("alpha"));
        assert_eq!(record.field("flowcraft.gen"), Some("7"));
    }

    #[test]
    fn captures_typed_values_as_text() {
        let capture = Capture::new();
        capture.with(|| {
            tracing::info!(ratio = 0.5, ok = true, "typed");
        });
        let record = capture.find("typed").expect("record");
        assert_eq!(record.field("ratio"), Some("0.5"));
        assert_eq!(record.field("ok"), Some("true"));
    }

    #[test]
    fn field_constants_match_what_call_sites_write() {
        let capture = Capture::new();
        capture.with(|| {
            tracing::info!(flowcraft.session = "s-1", "session");
        });
        let record = capture.find("session").expect("record");
        assert_eq!(record.field(attr::SESSION), Some("s-1"));
    }
}
