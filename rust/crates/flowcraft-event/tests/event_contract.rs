//! Replays the committed event contract against the port.
//!
//! `rust/conformance/event-contract.json` is dumped from `core/event` by
//! `tools/conformance-oracle -event-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half: it
//! parses the same subjects and patterns, matches the same pairs, and writes
//! the same envelopes, sentence by sentence.
//!
//! What it does not cover, and why: the bus. Which envelope reaches which
//! subscription (and with what timing guarantee) is the contract the bus slice
//! will pin, once the port's concurrency model is chosen; the routing keys and
//! the envelope wire are what every bus implementation underneath has to agree
//! on, and that is all of this file.
//!
//! One refusal is core's `encoding/json` rather than its own — an envelope
//! whose payload is not JSON fails inside `json.Marshal` — so that row is
//! marked and held to being a refusal, the same way the message half treats
//! those.

use flowcraft_event::envelope::Envelope;
use flowcraft_event::subject::{Pattern, Subject, producer_kind};
use flowcraft_event::time::Timestamp;
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/event-contract.json");

#[test]
fn the_event_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    for (table, floor) in [
        ("subjects", 12),
        ("patterns", 14),
        ("matches", 12),
        ("producers", 4),
        ("envelopes", 5),
    ] {
        let len = rows(&document, table).len();
        if len < floor {
            panic!("the contract has {len} rows in {table}, fewer than the {floor} it used to");
        }
    }

    for row in rows(&document, "subjects") {
        let subject = text_of(row, "subject");
        let parsed = Subject::parse(subject);
        if row["valid"].as_bool().unwrap_or(false) {
            if let Err(err) = parsed {
                failures.push(format!("subject {subject:?}: port refused with {err:?}"));
            }
        } else {
            match parsed {
                Ok(_) => failures.push(format!("subject {subject:?}: port accepted")),
                Err(err) if err == text_of(row, "error") => {}
                Err(err) => failures.push(format!(
                    "subject {subject:?}: port said {err:?}, core {:?}",
                    text_of(row, "error")
                )),
            }
        }
    }

    for row in rows(&document, "patterns") {
        let pattern = text_of(row, "pattern");
        let parsed = Pattern::parse(pattern);
        if row["valid"].as_bool().unwrap_or(false) {
            if let Err(err) = parsed {
                failures.push(format!("pattern {pattern:?}: port refused with {err:?}"));
            }
        } else {
            match parsed {
                Ok(_) => failures.push(format!("pattern {pattern:?}: port accepted")),
                Err(err) if err == text_of(row, "error") => {}
                Err(err) => failures.push(format!(
                    "pattern {pattern:?}: port said {err:?}, core {:?}",
                    text_of(row, "error")
                )),
            }
        }
    }

    for row in rows(&document, "matches") {
        let pattern = text_of(row, "pattern");
        let subject = text_of(row, "subject");
        let where_ = format!("match {pattern:?} against {subject:?}");
        let (Ok(pattern), Ok(subject)) = (Pattern::parse(pattern), Subject::parse(subject)) else {
            failures.push(format!("{where_}: the port cannot build both sides"));
            continue;
        };
        let matches = pattern.matches(&subject);
        if matches != row["matches"].as_bool().unwrap_or(false) {
            failures.push(format!(
                "{where_}: port says {matches}, core says {}",
                row["matches"]
            ));
        }
    }

    for row in rows(&document, "producers") {
        let raw = text_of(row, "subject");
        let Ok(subject) = Subject::parse(raw) else {
            failures.push(format!(
                "producer {raw:?}: the port cannot build the subject"
            ));
            continue;
        };
        if producer_kind(&subject) != text_of(row, "kind") {
            failures.push(format!(
                "producer {raw:?}: port says {:?}, core {:?}",
                producer_kind(&subject),
                text_of(row, "kind")
            ));
        }
    }

    for row in rows(&document, "envelopes") {
        let where_ = format!("envelope {:?}", text_of(row, "id"));
        let Ok(subject) = Subject::parse(text_of(row, "subject")) else {
            failures.push(format!("{where_}: the port cannot build the subject"));
            continue;
        };
        let mut envelope = Envelope::with_raw_payload(subject, text_of(row, "payload"));
        envelope.id = text_of(row, "id").to_owned();
        envelope.time = Timestamp::from_unix(
            row["time_secs"].as_i64().unwrap_or_default(),
            row["time_nanos"].as_u64().unwrap_or_default() as u32,
        );
        envelope.source = text_of(row, "source").to_owned();
        envelope.trace_id = text_of(row, "trace_id").to_owned();
        envelope.span_id = text_of(row, "span_id").to_owned();
        for (key, value) in row["headers"].as_object().into_iter().flatten() {
            envelope.set_header(key.clone(), value.as_str().unwrap_or_default());
        }
        match (row.get("wire"), envelope.to_wire_json()) {
            (Some(expected), Ok(wire)) => {
                let expected = expected.as_str().unwrap_or_default();
                if wire != expected {
                    failures.push(format!("{where_}: port wrote {wire:?}, core {expected:?}"));
                }
            }
            (Some(_), Err(err)) => {
                failures.push(format!("{where_}: port refused with {err:?}, core wrote"));
            }
            (None, Ok(wire)) => failures.push(format!(
                "{where_}: port wrote {wire:?}, core refused with {:?}",
                text_of(row, "wire_error")
            )),
            // The payload that is not JSON fails inside core's `json.Marshal`;
            // the port has its own sentence for it.
            (None, Err(_)) if text_of(row, "wire_text_from") == "encoding/json" => {}
            (None, Err(err)) if err == text_of(row, "wire_error") => {}
            (None, Err(err)) => failures.push(format!(
                "{where_}: port said {err:?}, core {:?}",
                text_of(row, "wire_error")
            )),
        }
    }

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(value) => value,
        Err(err) => panic!("the committed contract does not decode: {err}"),
    }
}

fn rows<'a>(document: &'a Value, table: &str) -> &'a Vec<Value> {
    match document[table].as_array() {
        Some(rows) => rows,
        None => panic!("{table} is not an array"),
    }
}

fn text_of<'a>(row: &'a Value, field: &str) -> &'a str {
    row.get(field).and_then(Value::as_str).unwrap_or_default()
}
