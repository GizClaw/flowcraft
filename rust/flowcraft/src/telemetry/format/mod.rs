//! Turning one event into one line.
//!
//! Both renderers consume the same [`Record`]: the field set is decided once,
//! so `text` and `json` can never disagree about what a record contains.

pub(crate) mod json;
pub(crate) mod text;
mod time;

use std::fmt;
use std::time::SystemTime;

use tracing::field::{Field as EventField, Visit};
use tracing::{Event, Level};

/// The implicit field name `tracing` gives a formatted message.
const MESSAGE: &str = "message";

/// One recorded value, kept typed so JSON can emit a number rather than a
/// string that looks like one.
#[derive(Clone)]
pub(crate) enum Value {
    Str(String),
    Bool(bool),
    I64(i64),
    U64(u64),
    F64(f64),
}

/// One `key=value` of an event.
#[derive(Clone)]
pub(crate) struct Field {
    pub(crate) name: String,
    pub(crate) value: Value,
}

/// A fully recorded event, before rendering.
#[derive(Clone)]
pub(crate) struct Record {
    pub(crate) at: SystemTime,
    pub(crate) level: Level,
    /// A tracing `target` is a `&'static str` by construction, so storing it as
    /// an owned `String` allocated once per record for no reason.
    pub(crate) target: &'static str,
    pub(crate) message: String,
    pub(crate) fields: Vec<Field>,
    /// `trace_id` and `span_id` of the context the record was emitted in.
    ///
    /// Read from the OpenTelemetry context, not from the fields: a call site
    /// cannot forget them, and a record outside any span has neither.
    pub(crate) trace: Option<(String, String)>,
}

/// The text substituted for a redacted value.
pub(crate) const REDACTED: &str = "<redacted>";

/// Collect an event's fields.
///
/// `tracing` records `%value` as `format_args!`, whose `Debug` forwards to
/// `Display`, so `record_debug` is the correct sink for both sigils and yields
/// the unquoted text the call site asked for.
///
/// With `redact`, any field whose *name* looks like a credential is replaced by
/// [`REDACTED`]. This is a backstop, not a licence: a call site must still not
/// pass a secret under a name that does not say so.
pub(crate) fn collect(event: &Event<'_>, redact: bool) -> Record {
    let mut visitor = Collector {
        message: String::new(),
        fields: Vec::new(),
        redact,
    };
    event.record(&mut visitor);
    let metadata = event.metadata();
    Record {
        at: SystemTime::now(),
        level: *metadata.level(),
        target: metadata.target(),
        message: visitor.message,
        fields: visitor.fields,
        #[cfg(feature = "otel")]
        trace: crate::telemetry::otel::ids(),
        #[cfg(not(feature = "otel"))]
        trace: None,
    }
}

struct Collector {
    message: String,
    fields: Vec<Field>,
    redact: bool,
}

impl Collector {
    fn push(&mut self, field: &EventField, value: Value) {
        if self.redact && looks_like_a_credential(field.name()) {
            self.fields.push(Field {
                name: field.name().to_owned(),
                value: Value::Str(REDACTED.to_owned()),
            });
            return;
        }
        if field.name() == MESSAGE {
            match &value {
                Value::Str(text) => self.message = text.clone(),
                other => self.message = render_scalar(other),
            }
            return;
        }
        self.fields.push(Field {
            name: field.name().to_owned(),
            value,
        });
    }
}

impl Visit for Collector {
    fn record_debug(&mut self, field: &EventField, value: &dyn fmt::Debug) {
        self.push(field, Value::Str(format!("{value:?}")));
    }

    fn record_str(&mut self, field: &EventField, value: &str) {
        self.push(field, Value::Str(value.to_owned()));
    }

    fn record_bool(&mut self, field: &EventField, value: bool) {
        self.push(field, Value::Bool(value));
    }

    fn record_i64(&mut self, field: &EventField, value: i64) {
        self.push(field, Value::I64(value));
    }

    fn record_u64(&mut self, field: &EventField, value: u64) {
        self.push(field, Value::U64(value));
    }

    fn record_f64(&mut self, field: &EventField, value: f64) {
        self.push(field, Value::F64(value));
    }

    fn record_error(&mut self, field: &EventField, value: &(dyn std::error::Error + 'static)) {
        self.push(field, Value::Str(value.to_string()));
    }
}

/// Whether a field name announces a credential.
fn looks_like_a_credential(name: &str) -> bool {
    let lowered = name.to_ascii_lowercase();
    crate::telemetry::attrs::SECRET_LIKE
        .iter()
        .any(|needle| lowered.contains(needle))
}

pub(crate) fn render_scalar(value: &Value) -> String {
    match value {
        Value::Str(text) => text.clone(),
        Value::Bool(flag) => flag.to_string(),
        Value::I64(number) => number.to_string(),
        Value::U64(number) => number.to_string(),
        Value::F64(number) => number.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    use tracing_subscriber::layer::SubscriberExt;

    /// Collects the same `Record` a renderer would see, with redaction on.
    #[derive(Clone, Default)]
    struct Probe(Arc<Mutex<Vec<Record>>>);

    struct ProbeLayer(Probe);

    impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for ProbeLayer {
        fn on_event(&self, event: &Event<'_>, _ctx: tracing_subscriber::layer::Context<'_, S>) {
            let record = collect(event, true);
            if let Ok(mut slot) = self.0.0.lock() {
                slot.push(record);
            }
        }
    }

    /// The record `body` emitted, as the renderer would have seen it.
    fn capture(body: impl FnOnce()) -> Record {
        let probe = Probe::default();
        let subscriber = tracing_subscriber::registry().with(ProbeLayer(probe.clone()));
        tracing::subscriber::with_default(subscriber, body);
        probe
            .0
            .lock()
            .ok()
            .and_then(|mut records| records.pop())
            .expect("one record")
    }

    fn field(record: &Record, name: &str) -> Option<String> {
        record
            .fields
            .iter()
            .find(|field| field.name == name)
            .map(|field| render_scalar(&field.value))
    }

    #[test]
    fn credential_names_are_detected() {
        for name in crate::telemetry::attrs::SECRET_LIKE {
            assert!(looks_like_a_credential(name), "{name}");
        }
        assert!(looks_like_a_credential("flowcraft.craft_token"));
        assert!(looks_like_a_credential("Authorization"));
        assert!(!looks_like_a_credential("flowcraft.craft"));
        assert!(!looks_like_a_credential("error.kind"));
    }

    #[test]
    fn a_credential_value_never_reaches_the_renderer() {
        let record = capture(|| {
            tracing::warn!(
                authorization = "Bearer super-secret",
                flowcraft.craft = "alpha",
                "denied"
            );
        });
        assert_eq!(field(&record, "authorization").as_deref(), Some(REDACTED));
        assert_eq!(field(&record, "flowcraft.craft").as_deref(), Some("alpha"));
        assert_eq!(record.message, "denied");
    }

    #[test]
    fn an_ordinary_field_is_left_alone() {
        let record = capture(|| {
            tracing::info!(path = "/srv/data", "opened");
        });
        assert_eq!(field(&record, "path").as_deref(), Some("/srv/data"));
    }
}
