//! One JSON object per record, one record per line.
//!
//! Flat, not nested: `flowcraft.craft` and `error.kind` are top-level keys, so
//! the same dotted name selects the same field whether it is queried in a
//! collector, grepped out of a file, or read by a support engineer. Nesting
//! would silently turn `flowcraft.craft` into `{"flowcraft": {"craft": ...}}`
//! and break that property.
//!
//! Numbers stay numbers and booleans stay booleans: a `flowcraft.gen` that
//! arrives as the string `"3"` cannot be compared against one that arrives as
//! the number `3`.

use std::fmt;
use std::sync::Arc;

use tracing::{Event, Level, Subscriber};
use tracing_subscriber::fmt::FmtContext;
use tracing_subscriber::fmt::format::{FormatEvent, Writer};
use tracing_subscriber::registry::LookupSpan;

use super::{Record, Value, collect, time};
use crate::resource::{Resource, Value as ResourceValue};

/// JSON formatter configuration.
pub(crate) struct Json {
    pub(crate) resource: Arc<Resource>,
    pub(crate) redact: bool,
    pub(crate) throttle: Option<Arc<crate::throttle::Throttle>>,
}

impl<S, N> FormatEvent<S, N> for Json
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

impl Json {
    /// Whether the record may be written, and what to say about the ones that
    /// were not.
    fn admit(&self, record: &Record) -> Option<u32> {
        match &self.throttle {
            Some(throttle) => throttle.admit(record),
            None => Some(0),
        }
    }

    /// Build the line for one record.
    fn render_line(&self, record: &Record, suppressed: u32) -> String {
        let mut line = String::with_capacity(192);
        line.push('{');
        line.push_str("\"ts\":");
        write_string(&mut line, &time::rfc3339_millis(record.at));
        line.push_str(",\"level\":");
        write_string(&mut line, level_name(record.level));
        line.push_str(",\"target\":");
        write_string(&mut line, &record.target);
        line.push_str(",\"msg\":");
        write_string(&mut line, &record.message);
        for (key, value) in self.resource.fields() {
            line.push(',');
            write_string(&mut line, key);
            line.push(':');
            match value {
                ResourceValue::Text(text) => write_string(&mut line, &text),
                ResourceValue::Count(number) => line.push_str(&number.to_string()),
            }
        }
        for field in &record.fields {
            line.push(',');
            write_string(&mut line, &field.name);
            line.push(':');
            write_value(&mut line, &field.value);
        }
        if suppressed > 0 {
            line.push_str(",\"suppressed\":");
            line.push_str(&suppressed.to_string());
        }
        if let Some((trace_id, span_id)) = &record.trace {
            line.push_str(",\"trace_id\":");
            write_string(&mut line, trace_id);
            line.push_str(",\"span_id\":");
            write_string(&mut line, span_id);
        }
        line.push_str("}\n");
        line
    }
}

fn level_name(level: Level) -> &'static str {
    level.as_str()
}

fn write_value(line: &mut String, value: &Value) {
    match value {
        Value::Str(text) => write_string(line, text),
        Value::Bool(flag) => line.push_str(if *flag { "true" } else { "false" }),
        Value::I64(number) => line.push_str(&number.to_string()),
        Value::U64(number) => line.push_str(&number.to_string()),
        Value::F64(number) if number.is_finite() => line.push_str(&number.to_string()),
        // JSON has no NaN or infinity; a null is honest about the loss.
        Value::F64(_) => line.push_str("null"),
    }
}

fn write_string(line: &mut String, value: &str) {
    line.push('"');
    for ch in value.chars() {
        match ch {
            '"' => line.push_str("\\\""),
            '\\' => line.push_str("\\\\"),
            '\n' => line.push_str("\\n"),
            '\r' => line.push_str("\\r"),
            '\t' => line.push_str("\\t"),
            other if (other as u32) < 0x20 || other == '\u{7f}' => {
                line.push_str(&format!("\\u{:04x}", other as u32));
            }
            other => line.push(other),
        }
    }
    line.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::format::Field;
    use std::time::UNIX_EPOCH;

    fn json() -> Json {
        Json {
            resource: Arc::new(Resource {
                service: "flowcraftd".to_owned(),
                version: "1.2.3".to_owned(),
                install: Some("01J8Z5Q2".to_owned()),
                install_scope: Some("machine".to_owned()),
                install_epoch: 4,
                session: "s-1".to_owned(),
                ..Resource::default()
            }),
            redact: true,
            throttle: None,
        }
    }

    fn record() -> Record {
        Record {
            at: UNIX_EPOCH,
            level: Level::WARN,
            target: "flowcraft_platformd::hosting".to_owned(),
            message: "craft grant renewal failed".to_owned(),
            trace: None,
            fields: vec![
                Field {
                    name: "flowcraft.gen".to_owned(),
                    value: Value::U64(3),
                },
                Field {
                    name: "error.kind".to_owned(),
                    value: Value::Str("not_available".to_owned()),
                },
                Field {
                    name: "cached".to_owned(),
                    value: Value::Bool(true),
                },
                Field {
                    name: "ratio".to_owned(),
                    value: Value::F64(f64::NAN),
                },
            ],
        }
    }

    #[test]
    fn is_one_parseable_object_with_typed_values() {
        let line = json().render_line(&record(), 0);
        assert!(line.ends_with("}\n"));
        let value: serde_json::Value = serde_json::from_str(&line).expect("valid json");
        assert_eq!(value["ts"], "1970-01-01T00:00:00.000Z");
        assert_eq!(value["level"], "WARN");
        assert_eq!(value["msg"], "craft grant renewal failed");
        assert_eq!(value["flowcraft.gen"], 3);
        assert_eq!(value["cached"], true);
        assert!(value["ratio"].is_null());
        assert_eq!(value["flowcraft.install"], "01J8Z5Q2");
        assert_eq!(value["flowcraft.install.epoch"], 4);
        assert_eq!(value["error.kind"], "not_available");
    }

    #[test]
    fn keys_are_flat() {
        let line = json().render_line(&record(), 0);
        let value: serde_json::Value = serde_json::from_str(&line).expect("valid json");
        assert!(
            value.get("flowcraft").is_none(),
            "dotted keys must not nest"
        );
        assert!(value.get("flowcraft.craft").is_none());
    }

    #[test]
    fn control_characters_are_escaped() {
        let mut record = record();
        record.message = "line\nbreak\ttab\u{1}".to_owned();
        let line = json().render_line(&record, 0);
        assert!(!line.trim_end().contains('\n'));
        let value: serde_json::Value = serde_json::from_str(&line).expect("valid json");
        assert_eq!(value["msg"], "line\nbreak\ttab\u{1}");
    }

    #[test]
    fn trace_ids_are_machine_readable_keys() {
        let mut record = record();
        record.trace = Some(("a".repeat(32), "b".repeat(16)));
        let line = json().render_line(&record, 0);
        let value: serde_json::Value = serde_json::from_str(&line).expect("valid json");
        assert_eq!(value["trace_id"].as_str().map(str::len), Some(32));
        assert_eq!(value["span_id"].as_str().map(str::len), Some(16));
    }

    #[test]
    fn quotes_and_backslashes_survive_a_round_trip() {
        let mut record = record();
        record.message = r#"a "quoted" path C:\temp"#.to_owned();
        let line = json().render_line(&record, 0);
        let value: serde_json::Value = serde_json::from_str(&line).expect("valid json");
        assert_eq!(value["msg"], record.message);
    }
}
