//! Streaming generation: the events a streaming call emits, and how they end.
//!
//! The wire here is the Responses API's own event stream, and three rules from the
//! design are what this module exists to enforce:
//!
//! - **`Finish` is the terminal event.** A stream that ends without one is a
//!   *truncated* provider stream, not a short answer — that is `ProviderTruncated` on
//!   the Go side, mapped there to `NotAvailable` because nothing about the caller's
//!   request was wrong. gRPC gives the same signal for free on the wire: the stream
//!   closes.
//! - **bookkeeping is not content.** `response.created`, `response.in_progress`, item
//!   registration and part bookkeeping are all dropped; only text deltas and the
//!   terminal event reach the caller.
//! - **a delta is not a part.** Text arrives in fragments, and the *runner* assembles
//!   them into the `Finish`'s canonical parts. A caller that concatenates deltas itself
//!   is a caller that will disagree with the assembled answer.
//!
//! The parse of the byte stream is `sse-stream` (already in the workspace through
//! `rmcp`): it is WHATWG-faithful, including the rule that a single LF after a CRLF is
//! skipped. What it does not know is this provider's event vocabulary, which is the
//! part below.

use bytes::Bytes;
use futures_util::Stream;
use futures_util::StreamExt as _;

use crate::error::{Error, Result};
use crate::inference::openai::OpenAi;
use crate::inference::request::GenerateResponse;

/// One event of a streaming generation.
#[derive(Debug, Clone, PartialEq)]
pub enum GenerateEvent {
    /// A fragment of the answer.
    Delta(String),
    /// The stream's terminal event: the assembled answer, its usage, why it stopped.
    Finish(GenerateResponse),
}

/// Turn a byte stream into canonical events.
///
/// `bytes` is the body of a streaming POST; it may be fragmented at any position, which
/// is the parser's whole job. Errors are terminal: the stream ends after one.
pub fn events<S, E>(bytes: S) -> impl Stream<Item = Result<GenerateEvent>> + Send
where
    S: Stream<Item = std::result::Result<Bytes, E>> + Send + 'static,
    E: std::error::Error + Send + Sync + 'static,
{
    let parser = sse_stream::SseStream::from_bytes_stream(bytes);
    futures_util::stream::unfold(
        State {
            parser: Box::pin(parser),
            finished: false,
            done: false,
        },
        |mut state| async move {
            if state.done {
                return None;
            }
            loop {
                match state.parser.next().await {
                    Some(Ok(event)) => match fold(&event) {
                        Step::Ignore => continue,
                        Step::Emit(event) => {
                            // A terminal event ends the stream: anything after it is a
                            // provider bug, and reading it would only confuse the
                            // assembly.
                            state.done = matches!(event, GenerateEvent::Finish(_));
                            state.finished |= state.done;
                            return Some((Ok(event), state));
                        }
                        Step::Fail(error) => {
                            state.done = true;
                            return Some((Err(error), state));
                        }
                    },
                    Some(Err(error)) => {
                        state.done = true;
                        return Some((
                            Err(Error::not_available(format!(
                                "inference: the provider stream is not valid SSE: {error}"
                            ))),
                            state,
                        ));
                    }
                    None => {
                        state.done = true;
                        if state.finished {
                            return None;
                        }
                        // No terminal event: the provider stopped talking before it said
                        // it was done. `provider_truncated` on the wire, `NotAvailable`
                        // in the taxonomy because the request itself was fine.
                        return Some((
                            Err(Error::not_available(
                                "inference: the provider stream ended before a terminal event \
                                 (provider_truncated)",
                            )),
                            state,
                        ));
                    }
                }
            }
        },
    )
}

struct State {
    parser: std::pin::Pin<
        Box<dyn Stream<Item = std::result::Result<sse_stream::Sse, sse_stream::Error>> + Send>,
    >,
    /// A terminal event was seen.
    finished: bool,
    /// Nothing more will be produced.
    done: bool,
}

enum Step {
    /// Bookkeeping: not content.
    Ignore,
    Emit(GenerateEvent),
    Fail(Error),
}

/// Fold one SSE event into the canonical stream.
fn fold(event: &sse_stream::Sse) -> Step {
    let Some(data) = event.data.as_deref() else {
        return Step::Ignore;
    };
    if data.trim().is_empty() {
        return Step::Ignore;
    }
    let Ok(value) = serde_json::from_str::<serde_json::Value>(data) else {
        return Step::Fail(Error::not_available(format!(
            "inference: a stream event is not JSON: {data}"
        )));
    };
    match value.get("type").and_then(serde_json::Value::as_str) {
        Some("response.output_text.delta") => match value
            .get("delta")
            .and_then(serde_json::Value::as_str)
        {
            // An empty delta is bookkeeping, not content.
            Some(delta) if !delta.is_empty() => Step::Emit(GenerateEvent::Delta(delta.to_owned())),
            _ => Step::Ignore,
        },
        Some("response.completed" | "response.incomplete") => {
            let Some(response) = value.get("response") else {
                return Step::Fail(Error::not_available(
                    "inference: a terminal stream event carries no response",
                ));
            };
            match OpenAi::new().decode(response) {
                Ok(response) => Step::Emit(GenerateEvent::Finish(response)),
                Err(error) => Step::Fail(error),
            }
        }
        Some("error") => Step::Fail(Error::not_available(format!(
            "inference: the provider reported an error mid-stream: {}",
            value
                .get("message")
                .and_then(serde_json::Value::as_str)
                .unwrap_or("no detail")
        ))),
        Some("response.failed") => Step::Fail(Error::not_available(format!(
            "inference: the response failed: {}",
            value
                .get("response")
                .and_then(|response| response.get("error"))
                .and_then(|error| error.get("message"))
                .and_then(serde_json::Value::as_str)
                .unwrap_or("no detail")
        ))),
        // Everything else is lifecycle bookkeeping: `response.created`,
        // `response.in_progress`, item and part registration, annotation and function-call
        // fragments this adapter does not lower yet.
        _ => Step::Ignore,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures_util::stream;

    /// Feed the bytes in pieces of `size`, so a `data:` line and its terminator are split
    /// across reads — the case a "read a line, then parse it" implementation fails.
    fn fragments(
        body: &str,
        size: usize,
    ) -> impl Stream<Item = std::result::Result<Bytes, std::io::Error>> + Send + 'static {
        let chunks: Vec<std::result::Result<Bytes, std::io::Error>> = body
            .as_bytes()
            .chunks(size)
            .map(|chunk| Ok(Bytes::copy_from_slice(chunk)))
            .collect();
        stream::iter(chunks)
    }

    async fn collect(body: &str, size: usize) -> Vec<std::result::Result<GenerateEvent, Error>> {
        events(fragments(body, size)).collect().await
    }

    fn delta(text: &str) -> String {
        format!("data: {{\"type\":\"response.output_text.delta\",\"delta\":\"{text}\"}}\r\n\r\n")
    }

    fn completed(text: &str) -> String {
        format!(
            "data: {{\"type\":\"response.completed\",\"response\":{{\
             \"output\":[{{\"type\":\"message\",\"content\":[{{\"type\":\"output_text\",\"text\":\"{text}\"}}]}}],\
             \"usage\":{{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}}}}\r\n\r\n"
        )
    }

    #[tokio::test]
    async fn deltas_then_a_terminal_event_finish_the_stream() {
        let body = format!(
            "event: response.created\r\ndata: {{\"type\":\"response.created\"}}\r\n\r\n{}{}",
            delta("hel"),
            delta("lo"),
        ) + &completed("hello");
        // Fragment every 7 bytes: mid-JSON, mid-terminator, everywhere.
        let events = collect(&body, 7).await;
        let mut texts = Vec::new();
        let mut finish = None;
        for event in events {
            match event.expect("no error") {
                GenerateEvent::Delta(text) => texts.push(text),
                GenerateEvent::Finish(response) => finish = Some(response),
            }
        }
        assert_eq!(texts, ["hel", "lo"]);
        let finish = finish.expect("a terminal event");
        assert_eq!(finish.usage.total_tokens, 4);
        assert_eq!(finish.finish, crate::inference::request::FinishReason::Stop);
    }

    /// The rule the design leans on: a stream that stops without saying it is done is a
    /// truncated provider stream, not a short answer.
    #[tokio::test]
    async fn a_stream_without_a_terminal_event_is_truncated() {
        let body = format!("{}{}", delta("half"), delta(" an answer"));
        let events = collect(&body, 11).await;
        let last = events.last().expect("at least one event");
        let error = last.as_ref().expect_err("truncated");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        assert!(error.to_string().contains("provider_truncated"), "{error}");
    }

    #[tokio::test]
    async fn bookkeeping_is_dropped_and_a_mid_stream_error_is_not() {
        let body = "data: {\"type\":\"response.in_progress\"}\r\n\r\n\
                    data: {\"type\":\"response.output_item.added\",\"output_index\":0}\r\n\r\n\
                    data: {\"type\":\"error\",\"message\":\"overloaded\"}\r\n\r\n";
        let events = collect(body, 5).await;
        assert_eq!(events.len(), 1, "{events:?}");
        let error = events[0].as_ref().expect_err("error");
        assert!(error.to_string().contains("overloaded"), "{error}");
    }

    /// One byte at a time is the slowest fragmentation there is; if the parser survives
    /// it, the terminator-spanning cases are covered.
    #[tokio::test]
    async fn a_stream_survives_one_byte_at_a_time() {
        let body = format!("{}{}", delta("x"), completed("x"));
        let events = collect(&body, 1).await;
        assert_eq!(events.len(), 2, "{events:?}");
        assert!(
            events[0]
                .as_ref()
                .expect("delta")
                .eq(&GenerateEvent::Delta("x".to_owned()))
        );
    }
}
