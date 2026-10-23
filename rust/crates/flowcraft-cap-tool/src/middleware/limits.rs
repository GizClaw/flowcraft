//! Result size limits and recoverable truncation.

use std::path::PathBuf;
use std::sync::Arc;

use flowcraft_error::{Error, Result};
use flowcraft_proto::message as msg;
use prost::Message as _;

use super::result_text;
use crate::executor::{Dispatch, Middleware};
use crate::tool::CallContext;

/// Appended to a result whose content exceeded the limit.
pub const DEFAULT_RESULT_MARKER: &str = "\n…[result truncated]";

/// The byte budget shared by non-text parts when none is configured.
pub const DEFAULT_PART_BUDGET: usize = 1 << 20;

/// Starts the pointer a spilled result carries.
pub const TRUNCATE_MARKER_PREFIX: &str = "\n…[truncated; full output: ";

/// The marker when nothing was persisted.
pub const TRUNCATE_MARKER: &str = "\n…[truncated]";

/// Options for [`result_limit`].
#[derive(Debug, Clone)]
pub struct ResultLimitOptions {
    /// Replaces [`DEFAULT_RESULT_MARKER`] when non-empty.
    pub marker: String,
    /// Byte budget for non-text parts. A zero budget lifts the cap.
    pub part_budget: usize,
}

impl Default for ResultLimitOptions {
    fn default() -> Self {
        Self {
            marker: DEFAULT_RESULT_MARKER.to_owned(),
            part_budget: DEFAULT_PART_BUDGET,
        }
    }
}

/// Cap how large a tool result may be.
///
/// Text is metered in runes and spent in order; text past the budget is dropped
/// and the marker lands where the cut happened. Non-text parts are metered by
/// encoded size against `part_budget`; a part that does not fit is dropped. The
/// `is_error` flag is preserved: truncation is a policy outcome, not an error.
pub fn result_limit(max_chars: usize, options: ResultLimitOptions) -> Result<Middleware> {
    if max_chars == 0 {
        return Err(Error::validation(
            "middleware.result_limit: max must be positive",
        ));
    }
    let marker = if options.marker.is_empty() {
        DEFAULT_RESULT_MARKER.to_owned()
    } else {
        options.marker
    };
    let part_budget = options.part_budget;
    Ok(Arc::new(move |next: Dispatch| {
        let marker = marker.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let marker = marker.clone();
            Box::pin(async move {
                let mut result = next(context, call).await;
                limit(&mut result, max_chars, &marker, part_budget);
                result
            })
        })
    }))
}

/// Cap only the non-text parts, leaving text unbounded.
pub fn result_part_limit(part_budget: usize) -> Result<Middleware> {
    result_limit(
        usize::MAX,
        ResultLimitOptions {
            part_budget,
            ..ResultLimitOptions::default()
        },
    )
}

fn limit(result: &mut msg::ToolResult, max_chars: usize, marker: &str, part_budget: usize) {
    let Some(content) = result.content.as_mut() else {
        return;
    };

    // Non-text parts first: their cut decides whether the marker needs room.
    let mut spent = 0usize;
    let mut media_cut = false;
    content.parts.retain(|part| {
        if matches!(part.kind, Some(msg::part::Kind::Text(_))) {
            return true;
        }
        let size = part.encoded_len();
        if part_budget > 0 && spent + size > part_budget {
            media_cut = true;
            false
        } else {
            spent += size;
            true
        }
    });

    let text_total: usize = content
        .parts
        .iter()
        .filter_map(|part| match part.kind.as_ref() {
            Some(msg::part::Kind::Text(text)) => Some(text.text.chars().count()),
            _ => None,
        })
        .sum();
    let cut = media_cut || text_total > max_chars;
    if !cut {
        return;
    }

    // The marker counts against the budget, so a cut always announces itself
    // without growing the result past `max_chars`.
    let marker_chars = marker.chars().count().min(max_chars);
    let mut remaining = max_chars.saturating_sub(marker_chars);
    for part in &mut content.parts {
        let Some(msg::part::Kind::Text(text)) = part.kind.as_mut() else {
            continue;
        };
        let count = text.text.chars().count();
        if remaining == 0 {
            text.text.clear();
        } else if count > remaining {
            text.text = text.text.chars().take(remaining).collect();
            remaining = 0;
        } else {
            remaining -= count;
        }
    }

    let trimmed: String = marker.chars().take(marker_chars).collect();
    if trimmed.is_empty() {
        return;
    }
    if let Some(text) = content
        .parts
        .iter_mut()
        .rev()
        .find_map(|part| match part.kind.as_mut() {
            Some(msg::part::Kind::Text(text)) => Some(text),
            _ => None,
        })
    {
        text.text.push_str(&trimmed);
    } else {
        content.parts.insert(
            0,
            msg::Part {
                kind: Some(msg::part::Kind::Text(msg::TextPart { text: trimmed })),
            },
        );
    }
}

/// Options for [`truncate`].
#[derive(Debug, Clone)]
pub struct TruncateOptions {
    /// The in-context cap, in Unicode code points.
    pub max_chars: usize,
    /// Where the full output is persisted.
    pub dir: PathBuf,
    /// Anchors the pointer written into the result; empty keeps it absolute.
    pub work_dir: Option<PathBuf>,
}

/// Cap oversized results recoverably: the full text is persisted under
/// `<dir>/<call_id>.output` and the in-context content becomes a head+tail
/// excerpt plus a pointer, so the model can read the rest on demand.
///
/// Results under the cap pass through untouched; error results are never
/// spilled. A failed spill does not fail the call: the result still leaves as an
/// excerpt, without a pointer.
pub fn truncate(options: TruncateOptions) -> Result<Middleware> {
    if options.max_chars == 0 {
        return Err(Error::validation(
            "middleware.truncate: max_chars must be positive",
        ));
    }
    if options.dir.as_os_str().is_empty() {
        return Err(Error::validation("middleware.truncate: dir is required"));
    }
    let options = Arc::new(options);
    Ok(Arc::new(move |next: Dispatch| {
        let options = options.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let options = options.clone();
            Box::pin(async move {
                let mut result = next(context, call).await;
                if result.is_error {
                    return result;
                }
                let text = result_text(&result);
                if text.chars().count() <= options.max_chars {
                    return result;
                }
                let pointer = spill(&options, &result.call_id, &text).await;
                let marker = match pointer {
                    Some(shown) => format!("{TRUNCATE_MARKER_PREFIX}{shown}]"),
                    None => TRUNCATE_MARKER.to_owned(),
                };
                let head_len = options.max_chars / 2;
                let tail_len = options.max_chars - head_len;
                let head: String = text.chars().take(head_len).collect();
                let tail_source: Vec<char> = text.chars().collect();
                let tail: String = tail_source[tail_source.len() - tail_len..].iter().collect();
                let excerpt = format!("{head}{marker}{tail}");
                if let Some(content) = result.content.as_mut() {
                    super::replace_text(content, excerpt);
                }
                result
            })
        })
    }))
}

async fn spill(options: &TruncateOptions, call_id: &str, text: &str) -> Option<String> {
    tokio::fs::create_dir_all(&options.dir).await.ok()?;
    let path = options.dir.join(format!("{}.output", sanitize(call_id)));
    tokio::fs::write(&path, text).await.ok()?;
    let shown = match options.work_dir.as_deref() {
        Some(base) => path
            .strip_prefix(base)
            .map(|path| path.display().to_string())
            .unwrap_or_else(|_| path.display().to_string()),
        None => path.display().to_string(),
    };
    Some(shown)
}

/// Keep a call id usable as a file name.
fn sanitize(call_id: &str) -> String {
    let name: String = call_id
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || matches!(character, '.' | '-' | '_') {
                character
            } else {
                '_'
            }
        })
        .collect();
    if name.trim_matches('_').is_empty() {
        "call".to_owned()
    } else {
        name
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::local::text_result;

    async fn run(
        middleware: Middleware,
        call: msg::ToolCall,
        result: msg::ToolResult,
    ) -> msg::ToolResult {
        let result = Arc::new(result);
        let core: Dispatch = Arc::new(move |_context, _call| {
            let result = result.clone();
            Box::pin(async move { (*result).clone() })
        });
        middleware(core)(CallContext::default(), call).await
    }

    fn call() -> msg::ToolCall {
        msg::ToolCall {
            id: "call-1".to_owned(),
            name: "echo".to_owned(),
            arguments: b"{}".to_vec(),
        }
    }

    #[tokio::test]
    async fn result_limit_cuts_text_and_marks_it() {
        let middleware = result_limit(40, ResultLimitOptions::default()).expect("limiter");
        let result = run(
            middleware,
            call(),
            msg::ToolResult {
                call_id: "call-1".to_owned(),
                content: Some(text_result("0123456789".repeat(10))),
                is_error: false,
            },
        )
        .await;
        let text = result_text(&result);
        assert!(text.starts_with("0123456789"), "{text:?}");
        assert!(text.contains("truncated"), "{text:?}");
        assert!(text.chars().count() <= 40, "{text:?}");
        assert!(!result.is_error);
    }

    #[tokio::test]
    async fn result_limit_drops_oversized_media_but_keeps_text() {
        let data = msg::Part {
            kind: Some(msg::part::Kind::Data(msg::DataPart {
                media_type: "application/json".to_owned(),
                value: vec![b'x'; 2048],
            })),
        };
        let middleware = result_limit(
            100,
            ResultLimitOptions {
                part_budget: 16,
                ..ResultLimitOptions::default()
            },
        )
        .expect("limiter");
        let result = run(
            middleware,
            call(),
            msg::ToolResult {
                call_id: "call-1".to_owned(),
                content: Some(msg::Content {
                    parts: vec![
                        msg::Part {
                            kind: Some(msg::part::Kind::Text(msg::TextPart {
                                text: "ok".to_owned(),
                            })),
                        },
                        data,
                    ],
                }),
                is_error: false,
            },
        )
        .await;
        let content = result.content.expect("content");
        assert!(
            content
                .parts
                .iter()
                .any(|part| matches!(part.kind, Some(msg::part::Kind::Text(_))))
        );
        assert!(
            !content
                .parts
                .iter()
                .any(|part| matches!(part.kind, Some(msg::part::Kind::Data(_))))
        );
    }

    #[tokio::test]
    async fn truncate_spills_and_leaves_a_pointer() {
        let dir = tempfile::tempdir().expect("tempdir");
        let middleware = truncate(TruncateOptions {
            max_chars: 10,
            dir: dir.path().to_path_buf(),
            work_dir: None,
        })
        .expect("truncate");
        let body = "0123456789abcdefghij";
        let result = run(
            middleware,
            call(),
            msg::ToolResult {
                call_id: "call-1".to_owned(),
                content: Some(text_result(body)),
                is_error: false,
            },
        )
        .await;
        let text = result_text(&result);
        assert!(text.contains("full output"), "{text:?}");
        let spilled = std::fs::read_to_string(dir.path().join("call-1.output")).expect("spill");
        assert_eq!(spilled, body);
    }

    #[tokio::test]
    async fn truncate_passes_short_and_error_results_through() {
        let dir = tempfile::tempdir().expect("tempdir");
        let options = TruncateOptions {
            max_chars: 100,
            dir: dir.path().to_path_buf(),
            work_dir: None,
        };
        let short = run(
            truncate(options.clone()).expect("truncate"),
            call(),
            msg::ToolResult {
                call_id: "call-1".to_owned(),
                content: Some(text_result("short")),
                is_error: false,
            },
        )
        .await;
        assert_eq!(result_text(&short), "short");

        let error = run(
            truncate(options).expect("truncate"),
            call(),
            msg::ToolResult {
                call_id: "call-1".to_owned(),
                content: Some(text_result("x".repeat(1000))),
                is_error: true,
            },
        )
        .await;
        assert!(error.is_error);
        assert_eq!(result_text(&error).chars().count(), 1000);
    }

    #[test]
    fn a_call_id_becomes_a_safe_file_name() {
        assert_eq!(sanitize("a/b c"), "a_b_c");
        assert_eq!(sanitize("___"), "call");
    }
}
