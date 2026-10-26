//! Built-in middleware: the policy layer around tool dispatch.
//!
//! Ported from `core/tool/middleware`. The chain mechanism lives in
//! [`crate::caps::tool::executor`]; this module provides the implementations, plus a
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

use crate::proto::message as msg;

/// Build an error result, the shape every short-circuiting middleware uses.
pub(crate) fn error_result(call_id: &str, message: impl Into<String>) -> msg::ToolResult {
    msg::ToolResult {
        call_id: call_id.to_owned(),
        content: Some(crate::message::text_content(message)),
        is_error: true,
    }
}

/// The text of a result, for logs and audit records.
///
/// The reading of "the text" is [`crate::message::text_of`]; this is only the
/// `ToolResult`-shaped door to it, because every caller here holds a result.
pub(crate) fn result_text(result: &msg::ToolResult) -> String {
    result
        .content
        .as_ref()
        .map_or_else(String::new, crate::message::text_of)
}
