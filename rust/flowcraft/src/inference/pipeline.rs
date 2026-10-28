//! The pipeline: compile, call, decode, and write down what happened.
//!
//! One unary call, in the order the design fixed: a driver lowers the canonical
//! request (no I/O), the transport performs the only I/O there is, the driver decodes
//! the answer, and the ledger records the attempt with the usage it produced. Routing
//! — which target, whether to retry, when to fall back — is a later layer; this is the
//! single-attempt body it will call.
//!
//! Splitting it this way is what lets every stage be tested without a socket: the
//! compile is a JSON snapshot, the decode is a canned payload, and the pipeline itself
//! runs against a recording transport.

use serde_json::Value;

use crate::error::{Error, Result};
use crate::inference::ledger::{Attempt, Ledger, Outcome, Phase, Trigger};
use crate::inference::model::{CompileReport, Operation};
use crate::inference::openai::OpenAi;
use crate::inference::request::{Compiled, GenerateRequest, GenerateResponse};
use crate::inference::transport::Transport;

/// What one call produced, and the evidence for it.
///
/// The ledger is *not* here: it belongs to whoever owns the retry policy, because it
/// accumulates across attempts and must survive a failure. [`generate`] writes into the
/// caller's ledger before it returns, so a failed call is still an audited one.
#[derive(Debug, Clone)]
pub struct Generation {
    /// The answer.
    pub response: GenerateResponse,
    /// Per-field evidence from the compile.
    pub report: CompileReport,
}

/// How a provider adapter is called.
///
/// `Send + Sync` because [`generate`] holds `&dyn Driver` across the transport await:
/// without it the future is not `Send` and cannot be spawned, which the
/// `future_not_send` lint reports the moment the pipeline is compiled.
pub trait Driver: Send + Sync {
    /// The path the body is posted to, relative to the deployment's base URL.
    fn url_path(&self) -> &'static str;

    /// Lower a canonical request. Pure: no I/O, no clock, no randomness.
    fn compile(&self, request: &GenerateRequest) -> Result<Compiled>;

    /// Decode one provider answer.
    fn decode(&self, raw: &Value) -> Result<GenerateResponse>;
}

/// Run one generation: compile, post, decode, record.
///
/// The ledger gets exactly one attempt, `succeeded` or `failed`, with the usage the
/// answer reported. A failure records the attempt too: an attempt that produced tokens
/// and then failed is on the bill, and a ledger that only knows about successes
/// under-reports it.
pub async fn generate(
    driver: &dyn Driver,
    transport: &dyn Transport,
    base_url: &str,
    headers: &[(&str, &str)],
    target: &str,
    request: &GenerateRequest,
    ledger: &mut Ledger,
) -> Result<Generation> {
    let compiled = driver.compile(request)?;
    let report = compiled.report.clone();
    let url = join_url(base_url, driver.url_path());
    let raw = transport.post_json(&url, headers, &compiled.body).await;
    let attempt = |outcome: Outcome, error_kind: Option<&str>| Attempt {
        target: target.to_owned(),
        phase: Phase::Execute,
        trigger: Trigger::Selection,
        outcome,
        error_kind: error_kind.map(str::to_owned),
        // Unary: nothing reaches the caller before the body does, so a failure here is
        // always safe to retry.
        observable_output: false,
        number: 1,
        backoff_millis: 0,
    };
    let raw = match raw {
        Ok(raw) => raw,
        Err(error) => {
            ledger.record(attempt(Outcome::Failed, Some(error.kind().as_str())), None);
            return Err(error);
        }
    };
    let response = match driver.decode(&raw) {
        Ok(response) => response,
        Err(error) => {
            // The provider answered and the answer was unusable: still an attempt, and
            // still whatever usage it reported.
            ledger.record(attempt(Outcome::Failed, Some(error.kind().as_str())), None);
            return Err(error);
        }
    };
    ledger.record(attempt(Outcome::Succeeded, None), Some(&response.usage));
    Ok(Generation { response, report })
}

/// Join a base URL and a path without producing a double slash.
fn join_url(base: &str, path: &str) -> String {
    let base = base.trim_end_matches('/');
    if path.starts_with('/') {
        format!("{base}{path}")
    } else {
        format!("{base}/{path}")
    }
}

/// The pipeline over the OpenAI Responses API.
///
/// A thin wrapper, and that is the point: the vendor adapter owns lowering and
/// decoding, and the pipeline owns the order and the record.
pub fn openai() -> OpenAiDriver {
    OpenAiDriver
}

/// [`Driver`] for the Responses API.
#[derive(Debug, Clone, Copy, Default)]
pub struct OpenAiDriver;

impl Driver for OpenAiDriver {
    fn url_path(&self) -> &'static str {
        "/v1/responses"
    }

    fn compile(&self, request: &GenerateRequest) -> Result<Compiled> {
        OpenAi::new().compile(request)
    }

    fn decode(&self, raw: &Value) -> Result<GenerateResponse> {
        OpenAi::new().decode(raw)
    }
}

/// The operation this pipeline currently serves. Named so a caller cannot assume the
/// others exist.
#[must_use]
pub fn served_operation() -> Operation {
    Operation::Generate
}

/// A pipeline error that is ours rather than the provider's.
#[must_use]
pub fn unimplemented(what: &str) -> Error {
    Error::not_available(format!(
        "inference: {what} is not implemented yet; only unary Generate is"
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::request::{TextIntent, ToolChoice};
    use crate::proto::message as msg;
    use serde_json::json;
    use std::sync::Mutex;

    /// A transport that records what it was asked to send and returns one fixed body.
    struct Recorder {
        body: Value,
        calls: Mutex<Vec<(String, Value)>>,
    }

    impl Recorder {
        fn new(body: Value) -> Self {
            Self {
                body,
                calls: Mutex::new(Vec::new()),
            }
        }
    }

    #[async_trait::async_trait]
    impl Transport for Recorder {
        async fn post_json(
            &self,
            url: &str,
            _headers: &[(&str, &str)],
            body: &Value,
        ) -> Result<Value> {
            self.calls
                .lock()
                .expect("lock")
                .push((url.to_owned(), body.clone()));
            Ok(self.body.clone())
        }
    }

    fn request() -> GenerateRequest {
        GenerateRequest {
            model: "gpt-5".to_owned(),
            messages: vec![msg::Message {
                role: msg::Role::User as i32,
                content: Some(msg::Content {
                    parts: vec![msg::Part {
                        kind: Some(msg::part::Kind::Text(msg::TextPart {
                            text: "hi".to_owned(),
                        })),
                    }],
                }),
            }],
            intent: TextIntent {
                max_output_tokens: Some(32),
                tool_choice: ToolChoice::Auto,
                ..TextIntent::default()
            },
        }
    }

    fn answer() -> Value {
        json!({
            "output": [{"type": "message", "content": [{"type": "output_text", "text": "hi"}]}],
            "usage": {"input_tokens": 5, "output_tokens": 2, "total_tokens": 7},
        })
    }

    #[tokio::test]
    async fn one_call_compiles_posts_decodes_and_records() {
        let transport = Recorder::new(answer());
        let mut ledger = Ledger::new();
        let generation = generate(
            &openai(),
            &transport,
            "https://api.example.com/",
            &[("authorization", "Bearer x")],
            "openai/gpt-5",
            &request(),
            &mut ledger,
        )
        .await
        .expect("generation");

        // The URL is the base plus the adapter's path, without a doubled slash.
        let calls = transport.calls.lock().expect("calls");
        assert_eq!(calls[0].0, "https://api.example.com/v1/responses");
        assert_eq!(calls[0].1["model"], "gpt-5");
        assert_eq!(calls[0].1["max_output_tokens"], 32);

        // The response, the report and the ledger all describe the same call.
        assert_eq!(generation.response.usage.total_tokens, 7);
        assert_eq!(ledger.attempts.len(), 1);
        assert_eq!(ledger.attempts[0].outcome, Outcome::Succeeded);
        assert_eq!(ledger.usage.total_tokens, 7);
        assert!(ledger.succeeded());
        generation
            .report
            .validate_success(
                Operation::Generate,
                &[
                    crate::inference::model::FieldId::GenerateExecutionUnary,
                    crate::inference::model::FieldId::GenerateContextRole,
                    crate::inference::model::FieldId::GenerateContextText,
                    crate::inference::model::FieldId::GenerateIntentTextMaxOutputTokens,
                ],
            )
            .expect("a complete report");
    }

    /// A transport failure is recorded as a failed attempt before it is returned: the
    /// ledger is the audit trail, and it must not lose the attempt that failed.
    #[tokio::test]
    async fn a_transport_failure_is_an_attempt_too() {
        struct Broken;
        #[async_trait::async_trait]
        impl Transport for Broken {
            async fn post_json(
                &self,
                _url: &str,
                _h: &[(&str, &str)],
                _b: &Value,
            ) -> Result<Value> {
                Err(Error::not_available("sandbox: the provider is unreachable"))
            }
        }
        let mut ledger = Ledger::new();
        let error = generate(
            &openai(),
            &Broken,
            "https://api.example.com",
            &[],
            "openai/gpt-5",
            &request(),
            &mut ledger,
        )
        .await
        .expect_err("failed");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        // The attempt that failed is in the ledger: an audit trail that only knows
        // about successes is the one that under-reports a bill.
        assert_eq!(ledger.attempts.len(), 1);
        assert_eq!(ledger.attempts[0].outcome, Outcome::Failed);
        assert_eq!(
            ledger.attempts[0].error_kind.as_deref(),
            Some("not_available")
        );
        assert!(!ledger.succeeded());
    }

    /// A provider answer that cannot be decoded is a failed attempt, not a panic and
    /// not a silent success.
    #[tokio::test]
    async fn a_body_that_is_not_a_response_fails_the_attempt() {
        let transport = Recorder::new(json!({"unexpected": true}));
        let mut ledger = Ledger::new();
        let error = generate(
            &openai(),
            &transport,
            "https://api.example.com",
            &[],
            "openai/gpt-5",
            &request(),
            &mut ledger,
        )
        .await
        .expect_err("undecodable");
        assert_eq!(error.kind(), crate::error::ErrorKind::Internal);
        assert_eq!(ledger.attempts.len(), 1, "the failed decode is an attempt");
    }
}
