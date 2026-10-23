//! Built-in middleware: the policy layer around tool dispatch.
//!
//! Ported from `core/tool/middleware`. The chain mechanism lives in
//! [`crate::executor`]; this module provides the implementations, plus a
//! settings parser for the declarative subset (`recover`, `telemetry`,
//! `result_limit`, `truncate`, `timeout`, `concurrency`). Approval, audit,
//! redaction and rate limiting stay programmatic because they need host
//! callbacks or a catalog.

mod limits;
mod policy;
mod runtime;
mod settings;

pub use limits::{
    DEFAULT_PART_BUDGET, DEFAULT_RESULT_MARKER, ResultLimitOptions, TRUNCATE_MARKER,
    TRUNCATE_MARKER_PREFIX, TruncateOptions, result_limit, result_part_limit, truncate,
};
pub use policy::{
    Approver, AuditRecord, AuditSink, RedactRule, approval, audit, audit_redacted, redact,
    redact_patterns,
};
pub use runtime::{
    concurrency, rate_limit, record_calls, recover, telemetry, timeout, timeout_with_catalog,
};
pub use settings::{
    ConcurrencySettings, RecoverSettings, ResultLimitSettings, Settings, TelemetrySettings,
    TimeoutSettings, TruncateSettings, from_settings, parse_duration,
};

use flowcraft_proto::message as msg;

/// Build an error result, the shape every short-circuiting middleware uses.
pub(crate) fn error_result(call_id: &str, message: impl Into<String>) -> msg::ToolResult {
    msg::ToolResult {
        call_id: call_id.to_owned(),
        content: Some(crate::local::text_result(message)),
        is_error: true,
    }
}

/// The text of a result, for logs and audit records.
pub(crate) fn result_text(result: &msg::ToolResult) -> String {
    let Some(content) = &result.content else {
        return String::new();
    };
    content
        .parts
        .iter()
        .filter_map(|part| match part.kind.as_ref() {
            Some(msg::part::Kind::Text(text)) => Some(text.text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("")
}

/// Mutably visit every text part.
pub(crate) fn for_each_text(content: &mut msg::Content, mut visit: impl FnMut(&mut msg::TextPart)) {
    for part in &mut content.parts {
        if let Some(msg::part::Kind::Text(text)) = part.kind.as_mut() {
            visit(text);
        }
    }
}

/// Replace the text parts of a content value with one text part, keeping the
/// non-text parts. Used by truncation, which rewrites the textual body.
pub(crate) fn replace_text(content: &mut msg::Content, text: String) {
    let mut parts = Vec::with_capacity(content.parts.len() + 1);
    parts.push(msg::Part {
        kind: Some(msg::part::Kind::Text(msg::TextPart { text })),
    });
    for part in content.parts.drain(..) {
        if !matches!(part.kind, Some(msg::part::Kind::Text(_))) {
            parts.push(part);
        }
    }
    content.parts = parts;
}
