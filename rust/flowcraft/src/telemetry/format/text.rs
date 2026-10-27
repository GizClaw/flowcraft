//! Human-readable, one line per record.
//!
//! Shape: `TIMESTAMP LEVEL target message key=value ...`
//!
//! The message is left unquoted (it is prose and stays readable), while every
//! other value is quoted only when it would otherwise be ambiguous. Control
//! characters are always escaped: one record must stay one line, or a
//! multi-line error message silently breaks every downstream parser.

use std::fmt;
use std::sync::Arc;

use tracing::{Event, Level, Subscriber};
use tracing_subscriber::fmt::FmtContext;
use tracing_subscriber::fmt::format::{FormatEvent, Writer};
use tracing_subscriber::registry::LookupSpan;

use super::{Record, Value, collect, time};
use crate::telemetry::resource::{Resource, Value as ResourceValue};

/// Text formatter configuration.
pub(crate) struct Text {
    pub(crate) timestamps: bool,
    pub(crate) targets: bool,
    pub(crate) ansi: bool,
    pub(crate) resource: Arc<Resource>,
    pub(crate) redact: bool,
    pub(crate) throttle: Option<Arc<crate::telemetry::throttle::Throttle>>,
}

impl<S, N> FormatEvent<S, N> for Text
where
    S: Subscriber + for<'a> LookupSpan<'a>,
    N: for<'a> tracing_subscriber::fmt::format::FormatFields<'a> + 'static,
{
    fn format_event(
        &self,
        _ctx: &FmtContext<'_, S, N>,
        mut writer: Writer<'_>,
        event: &Event<'_>,
    ) -> fmt::Result {
        let record = collect(event, self.redact);
        let Some(suppressed) = self.admit(&record) else {
            return Ok(());
        };
        let line = self.render_line(&record, suppressed);
        writer.write_str(&line)
    }
}

impl Text {
    /// Whether the record may be written, and what to say about the ones that
    /// were not.
    fn admit(&self, record: &Record) -> Option<u32> {
        match &self.throttle {
            Some(throttle) => throttle.admit(record),
            None => Some(0),
        }
    }

    /// Build the line for one record.
    ///
    /// Split out from [`FormatEvent`] so the same code path can be asserted
    /// without a subscriber.
    fn render_line(&self, record: &Record, suppressed: u32) -> String {
        let mut line = String::with_capacity(160);
        if self.timestamps {
            line.push_str(&time::rfc3339_millis(record.at));
            line.push(' ');
        }
        write_level(&mut line, record.level, self.ansi);
        if self.targets {
            line.push(' ');
            line.push_str(record.target);
        }
        line.push(' ');
        escape_into(&mut line, &record.message);
        for (key, value) in self.resource.fields() {
            line.push(' ');
            line.push_str(key);
            line.push('=');
            match value {
                ResourceValue::Text(text) => write_value(&mut line, &text),
                ResourceValue::Count(number) => line.push_str(&number.to_string()),
            }
        }
        for field in &record.fields {
            line.push(' ');
            line.push_str(&field.name);
            line.push('=');
            write_typed(&mut line, &field.value);
        }
        if suppressed > 0 {
            line.push_str(" suppressed=");
            line.push_str(&suppressed.to_string());
        }
        if let Some((trace_id, span_id)) = &record.trace {
            line.push_str(" trace_id=");
            line.push_str(trace_id);
            line.push_str(" span_id=");
            line.push_str(span_id);
        }
        line.push('\n');
        line
    }
}

fn write_level(line: &mut String, level: Level, ansi: bool) {
    let name = level.as_str();
    if !ansi {
        line.push_str(name);
        return;
    }
    let colour = match level {
        Level::TRACE => "35",
        Level::DEBUG => "34",
        Level::INFO => "32",
        Level::WARN => "33",
        Level::ERROR => "31",
    };
    line.push_str("\u{1b}[");
    line.push_str(colour);
    line.push('m');
    line.push_str(name);
    line.push_str("\u{1b}[0m");
}

fn write_typed(line: &mut String, value: &Value) {
    match value {
        Value::Str(text) => write_value(line, text),
        other => {
            line.push_str(&super::render_scalar(other));
        }
    }
}

/// Quote only when the bare form would be ambiguous.
fn write_value(line: &mut String, value: &str) {
    let needs_quotes = value.is_empty()
        || value
            .chars()
            .any(|ch| ch.is_whitespace() || ch == '=' || ch == '"' || ch.is_control());
    if !needs_quotes {
        line.push_str(value);
        return;
    }
    line.push('"');
    for ch in value.chars() {
        match ch {
            '"' => line.push_str("\\\""),
            '\\' => line.push_str("\\\\"),
            '\n' => line.push_str("\\n"),
            '\r' => line.push_str("\\r"),
            '\t' => line.push_str("\\t"),
            other if other.is_control() => {
                line.push_str(&format!("\\u{{{:x}}}", other as u32));
            }
            other => line.push(other),
        }
    }
    line.push('"');
}

/// Keep prose on one line without adding quotes.
fn escape_into(line: &mut String, text: &str) {
    for ch in text.chars() {
        match ch {
            '\n' => line.push_str("\\n"),
            '\r' => line.push_str("\\r"),
            '\t' => line.push(' '),
            other if other.is_control() => {
                line.push_str(&format!("\\u{{{:x}}}", other as u32));
            }
            other => line.push(other),
        }
    }
}

/// Render one collected record, for tests and for the support bundle.
#[cfg(test)]
fn render(text: &Text, record: &Record) -> String {
    let mut line = text.render_line(record, 0);
    line.pop();
    line
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::telemetry::format::{Field, Record};
    use std::time::UNIX_EPOCH;

    fn record(message: &str) -> Record {
        Record {
            at: UNIX_EPOCH,
            level: Level::INFO,
            target: "flowcraft_platformd",
            message: message.to_owned(),
            trace: None,
            fields: vec![
                Field {
                    name: "flowcraft.gen".to_owned(),
                    value: Value::U64(3),
                },
                Field {
                    name: "note".to_owned(),
                    value: Value::Str("two words".to_owned()),
                },
            ],
        }
    }

    fn text() -> Text {
        Text {
            timestamps: true,
            targets: true,
            ansi: false,
            resource: Arc::new(Resource {
                service: "flowcraftd".to_owned(),
                version: "1.2.3".to_owned(),
                session: "s-1".to_owned(),
                ..Resource::default()
            }),
            redact: true,
            throttle: None,
        }
    }

    #[test]
    fn renders_a_full_line() {
        let line = render(&text(), &record("generation swapped"));
        assert_eq!(
            line,
            "1970-01-01T00:00:00.000Z INFO flowcraft_platformd generation swapped \
             service.name=flowcraftd service.version=1.2.3 flowcraft.session=s-1 \
             flowcraft.gen=3 note=\"two words\""
        );
    }

    #[test]
    fn cli_profile_drops_timestamp_and_target() {
        let mut formatter = text();
        formatter.timestamps = false;
        formatter.targets = false;
        let line = render(&formatter, &record("ok"));
        assert!(line.starts_with("INFO ok "), "{line}");
        assert!(!line.contains("flowcraft_platformd"));
    }

    #[test]
    fn newlines_in_a_message_never_split_a_record() {
        let line = render(&text(), &record("first\nsecond"));
        assert!(!line.contains('\n'), "{line}");
        assert!(line.contains("first\\nsecond"));
    }

    #[test]
    fn empty_and_ambiguous_values_are_quoted() {
        let mut line = String::new();
        write_value(&mut line, "");
        assert_eq!(line, "\"\"");
        let mut line = String::new();
        write_value(&mut line, "a=b");
        assert_eq!(line, "\"a=b\"");
        let mut line = String::new();
        write_value(&mut line, "plain");
        assert_eq!(line, "plain");
    }

    #[test]
    fn a_trace_id_rides_on_the_record() {
        let mut record = record("exported");
        record.trace = Some(("a".repeat(32), "b".repeat(16)));
        let line = render(&text(), &record);
        assert!(line.contains("trace_id=aaaa"), "{line}");
        assert!(line.contains("span_id=bbbb"), "{line}");
    }

    #[test]
    fn ansi_wraps_only_the_level() {
        let mut formatter = text();
        formatter.ansi = true;
        let line = render(&formatter, &record("x"));
        assert!(line.contains("\u{1b}[32mINFO\u{1b}[0m"));
    }
}
