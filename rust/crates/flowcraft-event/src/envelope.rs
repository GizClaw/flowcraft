//! The envelope: one event's carrier, and the JSON a cross-process bus
//! forwards verbatim.
//!
//! Every field serialises cleanly. `payload` is JSON *text* -- the port's
//! `json.RawMessage` -- so an in-memory bus and a remote one behave the same
//! way (bytes in, bytes out, no `any`-typed surprises), server-side forwarding
//! can skip a re-encode, and consumers decide when to decode.
//!
//! The header names are constants for the same reason core has them: two
//! producers spelling `run_id` differently would silently stop correlating.

use std::collections::BTreeMap;

use flowcraft_contract::json;
use serde_json::Value;

use crate::id;
use crate::subject::Subject;
use crate::time::Timestamp;

/// The run this envelope belongs to.
pub const HEADER_RUN_ID: &str = "run_id";
/// The graph node this envelope belongs to.
pub const HEADER_NODE_ID: &str = "node_id";
/// The run that spawned this one (delegation, subagents); absent at the top.
pub const HEADER_PARENT_RUN_ID: &str = "parent_run_id";
/// The agent that produced this envelope -- the executor identity in
/// engine-neutral terms.
pub const HEADER_AGENT_ID: &str = "agent_id";
/// The tool call that spawned the run this envelope belongs to, when a tool
/// started it.
pub const HEADER_TOOL_CALL_ID: &str = "tool_call_id";
/// The graph this envelope belongs to.
pub const HEADER_GRAPH_ID: &str = "graph_id";
/// The tenant this envelope belongs to.
pub const HEADER_TENANT: &str = "tenant";

/// One event, ready to route.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Envelope {
    /// Unique per event; filled in by the producer or by the bus.
    pub id: String,
    /// Where it is routed.
    pub subject: Subject,
    /// When it was produced.
    pub time: Timestamp,
    /// Who produced it, when the producer says so.
    pub source: String,
    /// The producing trace, when there is one.
    pub trace_id: String,
    /// The producing span, when there is one.
    pub span_id: String,
    /// Correlation headers; see the constants above.
    pub headers: BTreeMap<String, String>,
    /// The event body, as JSON text. Empty means "no payload".
    pub payload: String,
}

impl Envelope {
    /// core's `NewEnvelope` with a `nil` payload: an id, the subject, and the
    /// time, and nothing else.
    pub fn new(subject: Subject) -> Envelope {
        Envelope {
            id: id::next(),
            subject,
            time: Timestamp::now(),
            source: String::new(),
            trace_id: String::new(),
            span_id: String::new(),
            headers: BTreeMap::new(),
            payload: String::new(),
        }
    }

    /// core's `NewEnvelope` with a value to encode: the payload is written the
    /// way `json.Marshal` writes it.
    pub fn with_payload(subject: Subject, payload: &Value) -> Envelope {
        let mut envelope = Envelope::new(subject);
        envelope.payload = json::to_string(payload);
        envelope
    }

    /// core's `NewEnvelope` with a `json.RawMessage` or `[]byte`: the caller
    /// already encoded it, and it is taken verbatim.
    pub fn with_raw_payload(subject: Subject, payload: impl Into<String>) -> Envelope {
        let mut envelope = Envelope::new(subject);
        envelope.payload = payload.into();
        envelope
    }

    /// Sets a header, the way core's `SetHeader` does.
    pub fn set_header(&mut self, key: impl Into<String>, value: impl Into<String>) {
        self.headers.insert(key.into(), value.into());
    }

    /// A header value, or `""` when absent.
    pub fn header(&self, key: &str) -> &str {
        self.headers.get(key).map(String::as_str).unwrap_or("")
    }

    /// `run_id`, when set.
    pub fn run_id(&self) -> &str {
        self.header(HEADER_RUN_ID)
    }

    /// `parent_run_id`, when set.
    pub fn parent_run_id(&self) -> &str {
        self.header(HEADER_PARENT_RUN_ID)
    }

    /// `tool_call_id`, when set.
    pub fn tool_call_id(&self) -> &str {
        self.header(HEADER_TOOL_CALL_ID)
    }

    /// `node_id`, when set.
    pub fn node_id(&self) -> &str {
        self.header(HEADER_NODE_ID)
    }

    /// `agent_id`, when set.
    pub fn agent_id(&self) -> &str {
        self.header(HEADER_AGENT_ID)
    }

    /// `graph_id`, when set.
    pub fn graph_id(&self) -> &str {
        self.header(HEADER_GRAPH_ID)
    }

    /// `tenant`, when set.
    pub fn tenant(&self) -> &str {
        self.header(HEADER_TENANT)
    }

    /// The wire form: core's struct, in field order, with the empty fields
    /// `omitempty` drops.
    ///
    /// `payload` is written compacted and HTML-escaped, which is what
    /// `encoding/json` does to a `json.RawMessage`; the text's own key order and
    /// number spelling are preserved, so a forwarded envelope is byte-identical
    /// to the one the producer sent.
    pub fn to_wire_json(&self) -> Result<String, String> {
        let mut out = String::from("{\"id\":");
        json::write_string(&self.id, &mut out);
        out.push_str(",\"subject\":");
        json::write_string(self.subject.as_str(), &mut out);
        out.push_str(",\"time\":");
        json::write_string(&self.time.to_rfc3339(), &mut out);
        for (name, value) in [
            ("source", self.source.as_str()),
            ("trace_id", self.trace_id.as_str()),
            ("span_id", self.span_id.as_str()),
        ] {
            if !value.is_empty() {
                out.push_str(&format!(",\"{name}\":"));
                json::write_string(value, &mut out);
            }
        }
        if !self.headers.is_empty() {
            out.push_str(",\"headers\":{");
            for (index, (key, value)) in self.headers.iter().enumerate() {
                if index > 0 {
                    out.push(',');
                }
                json::write_string(key, &mut out);
                out.push(':');
                json::write_string(value, &mut out);
            }
            out.push('}');
        }
        if !self.payload.is_empty() {
            if !is_valid_json(&self.payload) {
                return Err("event: payload is not valid JSON".into());
            }
            out.push_str(",\"payload\":");
            out.push_str(&json::compact(&self.payload));
        }
        out.push('}');
        Ok(out)
    }
}

/// Whether the text is one well-formed JSON value.
fn is_valid_json(payload: &str) -> bool {
    serde_json::from_str::<Value>(payload).is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn subject(value: &str) -> Subject {
        match Subject::parse(value) {
            Ok(subject) => subject,
            Err(err) => panic!("{value:?} should be a subject: {err}"),
        }
    }

    #[test]
    fn a_new_envelope_has_an_id_a_subject_and_a_time() {
        let envelope = Envelope::new(subject("graph.run.r1.start"));
        assert_eq!(envelope.id.len(), 20);
        assert!(!envelope.time.is_zero());
        assert!(envelope.payload.is_empty());
        assert_eq!(envelope.run_id(), "");
        let wire = match envelope.to_wire_json() {
            Ok(wire) => wire,
            Err(err) => panic!("{err}"),
        };
        // No payload, no headers: neither key appears.
        assert!(!wire.contains("\"payload\""));
        assert!(!wire.contains("\"headers\""));
    }

    #[test]
    fn headers_and_payload_are_written_go_s_way() {
        let mut envelope =
            Envelope::with_raw_payload(subject("kanban.board.b1.update"), r#"{ "b" : "<1>" }"#);
        envelope.id = "d5cf0k0g1h2j3k4m5n6o".into();
        envelope.time = Timestamp::from_unix(1_760_572_800, 0);
        envelope.set_header(HEADER_RUN_ID, "r1");
        envelope.set_header(HEADER_TENANT, "acme");
        assert_eq!(
            envelope.to_wire_json(),
            Ok(r#"{"id":"d5cf0k0g1h2j3k4m5n6o","subject":"kanban.board.b1.update","time":"2025-10-16T00:00:00Z","headers":{"run_id":"r1","tenant":"acme"},"payload":{"b":"\u003c1\u003e"}}"#.to_owned())
        );
    }

    #[test]
    fn a_payload_that_is_not_json_is_refused() {
        let envelope = Envelope::with_raw_payload(subject("a.b"), "not json");
        assert_eq!(
            envelope.to_wire_json(),
            Err("event: payload is not valid JSON".to_owned())
        );
    }
}
