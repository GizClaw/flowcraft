//! Result policies: redaction, audit, and approval.

use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::error::{Error, Result};
use crate::proto::message as msg;
use async_trait::async_trait;
use regex::Regex;

use super::error_result;
use crate::caps::tool::callable::CallContext;
use crate::caps::tool::executor::{Dispatch, Middleware};
use crate::message::for_each_text;

/// The replacement applied when a rule does not specify one.
pub const DEFAULT_REDACTION: &str = "[REDACTED]";

/// One regex-based redaction.
#[derive(Debug, Clone)]
pub struct RedactRule {
    /// The pattern to replace.
    pub pattern: Regex,
    /// The replacement; may reference capture groups with `$1` / `${name}`.
    pub replacement: String,
}

impl RedactRule {
    /// A rule replacing matches with [`DEFAULT_REDACTION`].
    pub fn new(pattern: &str) -> Result<Self> {
        Self::with_replacement(pattern, DEFAULT_REDACTION)
    }

    /// A rule with an explicit replacement.
    pub fn with_replacement(pattern: &str, replacement: impl Into<String>) -> Result<Self> {
        Ok(Self {
            pattern: Regex::new(pattern).map_err(|error| {
                Error::validation(format!("middleware.redact: {pattern:?}: {error}"))
            })?,
            replacement: replacement.into(),
        })
    }

    fn apply(&self, text: &str) -> String {
        self.pattern
            .replace_all(text, self.replacement.as_str())
            .into_owned()
    }
}

/// Rewrite result text through the rules before the model sees it. Call
/// arguments are untouched: the tool needs the real arguments, and the audit
/// path has its own redacting sink.
pub fn redact(rules: Vec<RedactRule>) -> Middleware {
    let rules = Arc::new(rules);
    Arc::new(move |next: Dispatch| {
        let rules = rules.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let rules = rules.clone();
            Box::pin(async move {
                let mut result = next(context, call).await;
                if let Some(content) = result.content.as_mut() {
                    for_each_text(content, |text| {
                        for rule in rules.iter() {
                            text.text = rule.apply(&text.text);
                        }
                    });
                }
                result
            })
        })
    })
}

/// Shorthand for [`redact`] with one pattern per rule.
pub fn redact_patterns(patterns: &[&str]) -> Result<Middleware> {
    let rules = patterns
        .iter()
        .map(|pattern| RedactRule::new(pattern))
        .collect::<Result<Vec<_>>>()?;
    Ok(redact(rules))
}

/// One audited call: what was asked, what came back, and how long it took.
///
/// Borrowed, because the sink is on the return path of every call: a record that
/// owned its fields made `audit` deep-copy the result (`result.clone()`), which
/// for a tool returning a media payload is megabytes per call, to hand it to a
/// sink that usually only reads it.
#[derive(Debug, Clone, Copy)]
pub struct AuditRecord<'a> {
    /// The call.
    pub call: &'a msg::ToolCall,
    /// The result.
    pub result: &'a msg::ToolResult,
    /// Time spent in the inner chain.
    pub duration: Duration,
}

/// Consumes audit records. Implementations must be safe for concurrent use and
/// must not block meaningfully; audit never breaks execution.
pub trait AuditSink: Send + Sync {
    /// Record one call.
    ///
    /// The record borrows: a sink that needs to keep it must copy what it needs,
    /// which is the only place a copy can still be justified.
    fn record(&self, record: AuditRecord<'_>);
}

/// Report every call and its outcome to the sink.
pub fn audit(sink: Arc<dyn AuditSink>) -> Middleware {
    Arc::new(move |next: Dispatch| {
        let sink = sink.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let sink = sink.clone();
            Box::pin(async move {
                let started = Instant::now();
                // `call` is moved into `next`, so the copy the sink sees is the
                // one that cannot be avoided. `result` stays here and is only
                // borrowed.
                let result = next(context, call.clone()).await;
                sink.record(AuditRecord {
                    call: &call,
                    result: &result,
                    duration: started.elapsed(),
                });
                result
            })
        })
    })
}

/// [`audit`] over a redacting wrapper: the record carries redacted copies,
/// while the model and the tool continue to see the originals.
pub fn audit_redacted(sink: Arc<dyn AuditSink>, rules: Vec<RedactRule>) -> Middleware {
    let rules = Arc::new(rules);
    Arc::new(move |next: Dispatch| {
        let sink = sink.clone();
        let rules = rules.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let sink = sink.clone();
            let rules = rules.clone();
            Box::pin(async move {
                let started = Instant::now();
                let result = next(context, call.clone()).await;
                // A copy is only made where a rule changes something: with no
                // match the record borrows the originals, which is the common
                // case for a payload nothing in the rule set mentions.
                let redacted_call = redact_call(&rules, &call);
                let redacted_result = redact_result(&rules, &result);
                sink.record(AuditRecord {
                    call: redacted_call.as_ref().unwrap_or(&call),
                    result: redacted_result.as_ref().unwrap_or(&result),
                    duration: started.elapsed(),
                });
                result
            })
        })
    })
}

/// A copy of `call` with its arguments redacted, or `None` when nothing matched.
fn redact_call(rules: &[RedactRule], call: &msg::ToolCall) -> Option<msg::ToolCall> {
    let arguments = String::from_utf8_lossy(&call.arguments);
    let redacted = redact_bytes(rules, &arguments);
    if redacted == arguments {
        return None;
    }
    let mut call = call.clone();
    call.arguments = redacted.into_bytes();
    Some(call)
}

/// A copy of `result` with its text redacted, or `None` when nothing matched.
fn redact_result(rules: &[RedactRule], result: &msg::ToolResult) -> Option<msg::ToolResult> {
    // Redact on a clone and report whether anything moved, so "no match" costs
    // one clone of the content and never touches the bytes.
    let mut redacted = result.clone();
    let content = redacted.content.as_mut()?;
    let mut changed = false;
    for_each_text(content, |text| {
        let next = redact_bytes(rules, &text.text);
        if next != text.text {
            text.text = next;
            changed = true;
        }
    });
    changed.then_some(redacted)
}

fn redact_bytes(rules: &[RedactRule], text: &str) -> String {
    let mut out = text.to_owned();
    for rule in rules {
        out = rule.apply(&out);
    }
    out
}

/// Decides whether a gated call may proceed.
#[async_trait]
pub trait Approver: Send + Sync {
    /// Approve (Ok) or deny (Err); the error message is shown to the model, so
    /// it should be human- and model-readable.
    async fn approve(&self, context: &CallContext, call: &msg::ToolCall) -> Result<()>;
}

/// Gate the named tools behind an approver. A denied call short-circuits with
/// an error result and never reaches the tool.
pub fn approval(approver: Arc<dyn Approver>, tools: Vec<String>) -> Middleware {
    let tools = Arc::new(tools);
    Arc::new(move |next: Dispatch| {
        let approver = approver.clone();
        let tools = tools.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let approver = approver.clone();
            let tools = tools.clone();
            Box::pin(async move {
                if !tools.iter().any(|name| name == &call.name) {
                    return next(context, call).await;
                }
                if let Err(error) = approver.approve(&context, &call).await {
                    return error_result(
                        &call.id,
                        format!("tool {:?} call denied: {error}", call.name),
                    );
                }
                next(context, call).await
            })
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::message::text_content;

    fn call(name: &str) -> msg::ToolCall {
        msg::ToolCall {
            id: "call-1".to_owned(),
            name: name.to_owned(),
            arguments: b"{\"token\":\"secret\"}".to_vec(),
        }
    }

    fn ok(result: msg::ToolResult) -> Dispatch {
        let result = Arc::new(result);
        Arc::new(move |_context, _call| {
            let result = result.clone();
            Box::pin(async move { (*result).clone() })
        })
    }

    fn result(text: &str) -> msg::ToolResult {
        msg::ToolResult {
            call_id: "call-1".to_owned(),
            content: Some(text_content(text)),
            is_error: false,
        }
    }

    #[tokio::test]
    async fn redact_rewrites_only_the_result() {
        let chain =
            redact(vec![RedactRule::new("secret").expect("rule")])(ok(result("token=secret")));
        let output = chain(CallContext::default(), call("echo")).await;
        assert_eq!(super::super::result_text(&output), "token=[REDACTED]");
    }

    /// What a sink kept: the borrowed record has to be copied, which is the one
    /// place a copy is still the sink's own choice.
    struct Seen {
        call: msg::ToolCall,
        result: msg::ToolResult,
        duration: Duration,
    }

    struct Collector(std::sync::Mutex<Vec<Seen>>);

    impl AuditSink for Collector {
        fn record(&self, record: AuditRecord<'_>) {
            if let Ok(mut records) = self.0.lock() {
                records.push(Seen {
                    call: record.call.clone(),
                    result: record.result.clone(),
                    duration: record.duration,
                });
            }
        }
    }

    /// The record is a view: `Copy` over two references is the compile-time proof
    /// that `audit` hands the sink the result it is about to return, and the
    /// size assertion is the proof that no payload travels inside it. A record
    /// that owned its fields would have made `audit` deep-copy every result.
    #[test]
    fn an_audit_record_is_a_view_not_a_copy() {
        let call = call("echo");
        let result = result("payload");
        let record = AuditRecord {
            call: &call,
            result: &result,
            duration: Duration::ZERO,
        };
        let copied = record;
        assert!(std::ptr::eq(copied.call, &call));
        assert!(std::ptr::eq(copied.result, &result));
        assert!(
            std::mem::size_of::<AuditRecord<'_>>() < 128,
            "the record must not carry the payload: {}",
            std::mem::size_of::<AuditRecord<'_>>()
        );
    }

    /// A rule set that matches nothing leaves both fields borrowed as they are,
    /// so the record the sink sees is the original result.
    #[tokio::test]
    async fn audit_redacted_records_the_originals_when_no_rule_matches() {
        let sink = Arc::new(Collector(std::sync::Mutex::new(Vec::new())));
        let chain = audit_redacted(sink.clone(), vec![RedactRule::new("absent").expect("rule")])(
            ok(result("token=secret")),
        );
        let output = chain(CallContext::default(), call("echo")).await;
        assert_eq!(super::super::result_text(&output), "token=secret");
        let records = sink.0.lock().expect("records");
        assert_eq!(records.len(), 1);
        // Nothing was redacted, so nothing was rewritten either.
        assert_eq!(
            super::super::result_text(&records[0].result),
            "token=secret"
        );
        assert!(String::from_utf8_lossy(&records[0].call.arguments).contains("secret"));
    }

    #[tokio::test]
    async fn audit_redacted_hides_secrets_in_the_record() {
        let sink = Arc::new(Collector(std::sync::Mutex::new(Vec::new())));
        let chain = audit_redacted(sink.clone(), vec![RedactRule::new("secret").expect("rule")])(
            ok(result("token=secret")),
        );
        let output = chain(CallContext::default(), call("echo")).await;
        // The model still sees the original result.
        assert_eq!(super::super::result_text(&output), "token=secret");
        let records = sink.0.lock().expect("records");
        assert_eq!(records.len(), 1);
        assert!(!String::from_utf8_lossy(&records[0].call.arguments).contains("secret"));
        assert_eq!(
            super::super::result_text(&records[0].result),
            "token=[REDACTED]"
        );
        // The record carries the timing of the inner chain, unchanged by the
        // borrowing.
        assert!(
            records[0].duration < Duration::from_secs(60),
            "{:?}",
            records[0].duration
        );
    }

    struct Deny;

    #[async_trait]
    impl Approver for Deny {
        async fn approve(&self, _context: &CallContext, _call: &msg::ToolCall) -> Result<()> {
            Err(Error::policy_denied("not allowed"))
        }
    }

    #[tokio::test]
    async fn approval_blocks_gated_tools_only() {
        let chain = approval(Arc::new(Deny), vec!["danger".to_owned()])(ok(result("ok")));
        let denied = chain(CallContext::default(), call("danger")).await;
        assert!(denied.is_error);
        assert!(super::super::result_text(&denied).contains("not allowed"));
        let allowed = chain(CallContext::default(), call("safe")).await;
        assert!(!allowed.is_error);
    }
}
