//! The local inference provider: the deployment's model table, over HTTP.
//!
//! The catalogue comes from the deployment document, not from the adapter: a platform that
//! guesses which models exist will happily route to one the operator never bought. Each
//! entry names the adapter (`openai`), the dialect (`response`), where it lives and how to
//! authenticate; everything else is the pipeline's.
//!
//! Settings shape:
//!
//! ```yaml
//! models:
//!   - name: gpt-5
//!     provider: openai              # the only adapter this build has
//!     api: response                 # the only dialect it lowers
//!     base_url: https://api.openai.com
//!     api_key: ${secret:openai.key} # literal or a resolver reference
//!     max_output_tokens: 4096       # ceiling applied to a request that asks none
//!
//! tiers:                            # which model a named class runs against, in order
//!   standard: [gpt-5, gpt-5-mini]   # a name, or `{model: gpt-5}`
//! retry:
//!   max_attempts: 2                 # per target, including the first
//!   seed_ms: 100                    # first backoff, doubled per attempt
//!   cap_ms: 2000
//!   fallback_on_retry_exhausted: false
//!   max_total_attempts: 0           # 0 = no cap across targets
//! breaker:
//!   threshold: 3                    # consecutive failures that open a target
//!   cooldown_ms: 30000
//! ```
//!
//! A tier may only name a model the same document declares: a table that promises a class
//! is a promise the deployment can serve, and the build refuses a name it cannot.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde::Deserialize;

use crate::caps::inference::contract::{
    CONTRACT, Capabilities, EventStream, Inference, KIND, ModelDescriptor,
};
use crate::error::{Error, Result};
use crate::inference::active::active_fields;
use crate::inference::http::HttpTransport;
use crate::inference::model::{CompileReport, Operation};
use crate::inference::openai::{API_RESPONSES, OpenAi, PROVIDER};
use crate::inference::pipeline::{Generation, generate};
use crate::inference::request::GenerateRequest;
use crate::inference::stream::events;
use crate::inference::transport::Transport;
use crate::resource::{BuildCtx, Factory, Kind, Phase, Spec};

/// The provider id this capability registers under.
const IMPL: &str = "local";

/// One declared model.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct ModelSettings {
    /// The name a request uses.
    name: String,
    /// The adapter. `openai` is the only one this build has.
    #[serde(default = "default_provider")]
    provider: String,
    /// The dialect. `response` is the only one lowered; `chat` would be a second lowering
    /// of the same request with its own losses, and it does not exist yet.
    #[serde(default = "default_api")]
    api: String,
    /// Where the provider lives.
    base_url: String,
    /// The credential, as a literal or a `${secret:…}` reference the resolver expands.
    #[serde(default)]
    api_key: String,
    /// A ceiling applied to a request that asks for none. Zero means "no ceiling here".
    #[serde(default)]
    max_output_tokens: Option<u64>,
    /// Per-call deadline. A provider that never answers must not hold a caller forever.
    #[serde(default = "default_timeout")]
    timeout_secs: u64,
}

fn default_provider() -> String {
    PROVIDER.to_owned()
}

fn default_api() -> String {
    API_RESPONSES.to_owned()
}

fn default_timeout() -> u64 {
    120
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    models: Vec<ModelSettings>,
    /// Routing tiers, by name, in the deployment's order.
    ///
    /// Declared here rather than in the adapter because the order is the operator's: a
    /// provider that ranked its own models would be choosing what the deployment bought.
    #[serde(default)]
    tiers: BTreeMap<String, Vec<crate::inference::route::Target>>,
    /// Retry behaviour, defaulting to one attempt.
    #[serde(default)]
    retry: crate::inference::route::RetryPolicy,
    /// Breaker behaviour.
    #[serde(default)]
    breaker: crate::inference::route::BreakerPolicy,
}

/// One model, resolved and ready to call.
///
/// Public because a host that brings its own transport builds these: [`Local`] serves the
/// deployment's table, and the table is not the transport's business.
#[derive(Debug, Clone)]
pub struct Model {
    /// Where the provider lives, without a trailing slash.
    pub base_url: String,
    /// The credential, already resolved by the settings resolver. Empty means "no
    /// authorization header", which is a local or an unauthenticated endpoint.
    pub api_key: String,
    /// A ceiling applied to a request that asks for none.
    pub max_output_tokens: Option<u64>,
    /// How long one call may take.
    pub timeout: Duration,
}

/// The local provider.
pub struct Local {
    models: BTreeMap<String, Model>,
    transport: Arc<dyn Transport>,
    routing: crate::inference::route::Policy,
}

impl std::fmt::Debug for Local {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("inference.Local")
            .field("models", &self.models.keys().collect::<Vec<_>>())
            .finish_non_exhaustive()
    }
}

impl Local {
    /// A provider over an explicit transport, for a caller that brings its own client.
    #[must_use]
    pub fn with_transport(models: BTreeMap<String, Model>, transport: Arc<dyn Transport>) -> Self {
        Self {
            models,
            transport,
            routing: crate::inference::route::Policy::default(),
        }
    }

    /// The routing table this deployment declared.
    ///
    /// A host that brings its own transport can still take the tiers from here: routing is a
    /// decision about the operator's order, and the transport has no opinion about it.
    #[must_use]
    pub fn routing(&self) -> &crate::inference::route::Policy {
        &self.routing
    }

    /// Route a call through this deployment's table.
    ///
    /// [`Local`] is a [`crate::inference::route::Generator`] over its own models, so the
    /// table it declares is the table it can serve — and a tier that names a model this
    /// deployment does not declare is refused at build time, not discovered at 3 a.m.
    pub async fn route(
        &self,
        choice: crate::inference::route::Choice,
        request: crate::inference::request::GenerateRequest,
        ledger: &mut crate::inference::ledger::Ledger,
    ) -> Result<crate::inference::pipeline::Generation> {
        let route = crate::inference::route::Route::with_clock(
            self.routing.clone(),
            Box::new(std::time::Instant::now),
        );
        route.generate(self, choice, request, ledger).await
    }

    /// The model a request names, or `NotFound`: an undeclared model is a deployment the
    /// operator never bought, not something to guess at.
    fn model(&self, name: &str) -> Result<&Model> {
        self.models.get(name).ok_or_else(|| {
            Error::not_found(format!(
                "inference: model {name:?} is not declared in this deployment"
            ))
        })
    }

    /// The intent to compile, with the deployment's ceiling applied where the request has
    /// none.
    fn intent_for<'a>(
        &self,
        model: &Model,
        request: &'a GenerateRequest,
    ) -> std::borrow::Cow<'a, crate::inference::request::TextIntent> {
        use std::borrow::Cow;
        match (request.intent.max_output_tokens, model.max_output_tokens) {
            (None, Some(ceiling)) => {
                let mut intent = request.intent.clone();
                intent.max_output_tokens = Some(ceiling);
                Cow::Owned(intent)
            }
            _ => Cow::Borrowed(&request.intent),
        }
    }
}

#[async_trait]
impl Inference for Local {
    fn capabilities(&self) -> crate::resource::BoxFuture<'_, Result<Capabilities>> {
        let models = self.models.len();
        Box::pin(async move {
            Ok(Capabilities {
                operations: vec![Operation::Generate],
                streams: true,
                models,
            })
        })
    }

    async fn models(&self) -> Result<Vec<ModelDescriptor>> {
        Ok(self
            .models
            .keys()
            .map(|name| ModelDescriptor {
                name: name.clone(),
                provider: PROVIDER.to_owned(),
                api: API_RESPONSES.to_owned(),
            })
            .collect())
    }

    async fn explain(&self, request: &GenerateRequest) -> Result<CompileReport> {
        let model = self.model(&request.model)?;
        // The effective request, not the caller's: the deployment's ceiling is part of what
        // gets compiled, so it is part of what the report must cover.
        let effective = GenerateRequest {
            model: request.model.clone(),
            messages: request.messages.clone(),
            intent: self.intent_for(model, request).into_owned(),
        };
        let compiled = OpenAi::new().compile(&effective)?;
        // The same rule the pipeline enforces, asked here so a compile that would fail at
        // call time fails at explain time instead.
        compiled
            .report
            .validate_success(Operation::Generate, &active_fields(&effective))?;
        Ok(compiled.report)
    }

    async fn generate(&self, request: GenerateRequest) -> Result<Generation> {
        let model = self.model(&request.model)?;
        // Owned before `messages` moves: the deployment's ceiling is applied to a copy,
        // so the borrowed view cannot outlive the request it points at.
        let intent = self.intent_for(model, &request).into_owned();
        let request = GenerateRequest {
            model: request.model.clone(),
            messages: request.messages,
            intent,
        };
        let mut ledger = crate::inference::ledger::Ledger::new();
        let bearer = authorization(model);
        let headers: Vec<(&str, &str)> = bearer
            .as_deref()
            .map(|value| vec![("authorization", value)])
            .unwrap_or_default();
        let generation = generate(
            &crate::inference::pipeline::openai(),
            self.transport.as_ref(),
            &model.base_url,
            &headers,
            &request.model,
            &request,
            &mut ledger,
        )
        .await?;
        Ok(generation)
    }

    async fn generate_stream(&self, request: GenerateRequest) -> Result<EventStream> {
        let model = self.model(&request.model)?;
        if !self.capabilities().await?.streams {
            return Err(Error::not_available(
                "inference: this provider cannot stream",
            ));
        }
        let intent = self.intent_for(model, &request).into_owned();
        let request = GenerateRequest {
            model: request.model.clone(),
            messages: request.messages,
            intent,
        };
        let compiled = crate::inference::pipeline::Driver::compile(
            &crate::inference::pipeline::openai(),
            &request,
        )?;
        compiled
            .report
            .validate_success(Operation::Generate, &active_fields(&request))?;
        let bearer = authorization(model);
        let headers: Vec<(&str, &str)> = bearer
            .as_deref()
            .map(|value| vec![("authorization", value)])
            .unwrap_or_default();
        let bytes = self
            .transport
            .post_stream(&model.base_url, &headers, &compiled.body)
            .await?;
        Ok(Box::pin(events(bytes)))
    }
}

/// The `authorization` value for a model, or nothing when the deployment named no key.
///
/// Returned as the bare string so the call sites can borrow it: the transport takes
/// `&[(&str, &str)]`, and a formatted header has to live somewhere.
fn authorization(model: &Model) -> Option<String> {
    if model.api_key.is_empty() {
        return None;
    }
    Some(format!("Bearer {}", model.api_key))
}

/// The `inference.Provider/local` factory.
pub struct LocalFactory;

impl Factory for LocalFactory {
    type Output = crate::caps::inference::contract::Handle;

    fn spec(&self) -> Spec {
        Spec {
            kind: Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}")),
            imp: Some(IMPL),
            contract: Some(CONTRACT),
            deps: Vec::new(),
            phase: Phase::Pre,
            validate_settings: Some(validate),
        }
    }

    fn build<'a>(
        &'a self,
        ctx: BuildCtx<'a>,
    ) -> crate::resource::BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let mut models = BTreeMap::new();
            for model in &settings.models {
                models.insert(
                    model.name.clone(),
                    Model {
                        base_url: model.base_url.trim_end_matches('/').to_owned(),
                        api_key: model.api_key.clone(),
                        max_output_tokens: model.max_output_tokens.filter(|limit| *limit > 0),
                        timeout: Duration::from_secs(model.timeout_secs),
                    },
                );
            }
            let secure = settings
                .models
                .iter()
                .any(|model| model.base_url.starts_with("https://"));
            let transport: Arc<dyn Transport> = if secure {
                Arc::new(HttpTransport::with_tls(system_roots()?))
            } else {
                Arc::new(HttpTransport::plain())
            };
            Ok(Arc::new(crate::caps::inference::contract::Handle::new(
                Arc::new(Local {
                    models,
                    transport,
                    routing: crate::inference::route::Policy {
                        tiers: settings.tiers,
                        retry: settings.retry,
                        breaker: settings.breaker,
                    },
                }),
            )))
        })
    }
}

/// The host's own trust store, because a provider endpoint is a public name.
fn system_roots() -> Result<tokio_rustls::rustls::RootCertStore> {
    let mut roots = tokio_rustls::rustls::RootCertStore::empty();
    let loaded = rustls_native_certs::load_native_certs();
    for cert in loaded.certs {
        let _ = roots.add(cert);
    }
    if roots.is_empty() {
        return Err(Error::not_available(
            "inference: no system trust store is available, so an https provider endpoint \
             cannot be verified",
        ));
    }
    Ok(roots)
}

/// Routing drives a provider through this one call, so a deployment's table and the models
/// it can serve cannot disagree: both are the same [`Local`].
#[async_trait::async_trait]
impl crate::inference::route::Generator for Local {
    async fn generate(&self, request: GenerateRequest) -> Result<Generation> {
        <Self as Inference>::generate(self, request).await
    }
}

fn validate(value: &serde_json::Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.models.is_empty() {
        return Err(Error::validation(
            "inference: settings.models must declare at least one model",
        ));
    }
    let mut seen = std::collections::BTreeSet::new();
    for (index, model) in settings.models.iter().enumerate() {
        if model.name.trim().is_empty() {
            return Err(Error::validation(format!(
                "inference: models[{index}].name is required"
            )));
        }
        if model.name.trim() != model.name {
            return Err(Error::validation(format!(
                "inference: models[{index}].name {:?} has surrounding whitespace",
                model.name
            )));
        }
        if !seen.insert(model.name.as_str()) {
            return Err(Error::validation(format!(
                "inference: models[{index}].name {:?} is declared twice",
                model.name
            )));
        }
        if model.provider != PROVIDER {
            return Err(Error::validation(format!(
                "inference: models[{index}].provider {:?} is not one this build has ({PROVIDER})",
                model.provider
            )));
        }
        if model.api != API_RESPONSES {
            return Err(Error::validation(format!(
                "inference: models[{index}].api {:?} is not lowered; only {API_RESPONSES} is",
                model.api
            )));
        }
        if !(model.base_url.starts_with("https://") || model.base_url.starts_with("http://")) {
            return Err(Error::validation(format!(
                "inference: models[{index}].base_url {:?} has no scheme",
                model.base_url
            )));
        }
        if model.timeout_secs == 0 {
            return Err(Error::validation(format!(
                "inference: models[{index}].timeout_secs must be positive"
            )));
        }
    }
    validate_tiers(&settings)?;
    Ok(())
}

/// A tier is a promise the deployment can serve: every name in it must be a declared model,
/// and a name repeated in one tier is a retry budget spent twice on the same provider.
fn validate_tiers(settings: &Settings) -> Result<()> {
    let declared: std::collections::BTreeSet<&str> = settings
        .models
        .iter()
        .map(|model| model.name.as_str())
        .collect();
    for (tier, targets) in &settings.tiers {
        if tier.trim().is_empty() {
            return Err(Error::validation(
                "inference: a tier name must not be empty",
            ));
        }
        if targets.is_empty() {
            return Err(Error::validation(format!(
                "inference: tier {tier:?} declares no target"
            )));
        }
        let mut seen = std::collections::BTreeSet::new();
        for target in targets {
            if !declared.contains(target.model.as_str()) {
                return Err(Error::validation(format!(
                    "inference: tier {tier:?} names model {:?}, which models: does not declare",
                    target.model
                )));
            }
            if !seen.insert(target.model.as_str()) {
                return Err(Error::validation(format!(
                    "inference: tier {tier:?} names model {:?} twice",
                    target.model
                )));
            }
        }
    }
    if settings.retry.max_attempts > 1 && settings.retry.seed > settings.retry.cap {
        return Err(Error::validation(
            "inference: retry.seed_ms must not exceed retry.cap_ms",
        ));
    }
    if settings.breaker.cooldown.is_zero() && settings.breaker.threshold > 0 {
        return Err(Error::validation(
            "inference: breaker.cooldown_ms must be positive when breaker.threshold is set",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::request::{TextIntent, ToolChoice};
    use crate::proto::message as msg;
    use serde_json::json;

    fn settings(models: serde_json::Value) -> serde_json::Value {
        json!({ "models": models })
    }

    fn one_model(name: &str) -> serde_json::Value {
        json!([{
            "name": name,
            "provider": "openai",
            "api": "response",
            "base_url": "https://api.openai.com",
        }])
    }

    /// The tier order is the deployment's, and it is read as written — not sorted, not
    /// deduplicated, not reinterpreted.
    #[test]
    fn a_tier_is_read_in_the_declared_order() {
        let value = json!({
            "models": [{
                "name": "gpt-5",
                "provider": "openai",
                "api": "response",
                "base_url": "https://api.openai.com",
            }, {
                "name": "gpt-5-mini",
                "provider": "openai",
                "api": "response",
                "base_url": "https://api.openai.com",
            }],
            "tiers": {
                "standard": ["gpt-5-mini", {"model": "gpt-5"}],
            },
            "retry": {"max_attempts": 2, "seed_ms": 10, "cap_ms": 100},
            "breaker": {"threshold": 2, "cooldown_ms": 500},
        });
        validate(&value).expect("a well-formed table");
        let settings: Settings = crate::resource::settings(&value).expect("parse");
        let standard = &settings.tiers["standard"];
        assert_eq!(
            standard
                .iter()
                .map(|target| target.model.as_str())
                .collect::<Vec<_>>(),
            vec!["gpt-5-mini", "gpt-5"],
            "the short and the long spelling mean the same target, in the same place"
        );
        assert_eq!(settings.retry.max_attempts, 2);
        assert_eq!(settings.retry.seed, std::time::Duration::from_millis(10));
        assert_eq!(settings.retry.cap, std::time::Duration::from_millis(100));
        assert_eq!(settings.breaker.threshold, 2);
        assert_eq!(
            settings.breaker.cooldown,
            std::time::Duration::from_millis(500)
        );
    }

    /// A tier that names a model the deployment never bought is the failure this validation
    /// exists for: the runtime would otherwise discover it as a call to nowhere.
    #[test]
    fn a_tier_naming_an_undeclared_model_is_refused() {
        let value = json!({
            "models": one_model("gpt-5"),
            "tiers": {"standard": ["gpt-5", "claude"]},
        });
        let error = validate(&value).expect_err("claude is not declared");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(error.message().contains("claude"), "{}", error.message());
    }

    #[test]
    fn a_tier_listing_one_model_twice_is_refused() {
        let value = json!({
            "models": one_model("gpt-5"),
            "tiers": {"standard": ["gpt-5", "gpt-5"]},
        });
        let error = validate(&value).expect_err("a repeated target");
        assert!(error.message().contains("twice"), "{}", error.message());
    }

    #[test]
    fn an_empty_tier_and_a_backwards_backoff_are_refused() {
        let empty = json!({
            "models": one_model("gpt-5"),
            "tiers": {"standard": []},
        });
        assert!(
            validate(&empty).is_err(),
            "an empty tier has nothing to serve"
        );
        let backwards = json!({
            "models": one_model("gpt-5"),
            "retry": {"max_attempts": 2, "seed_ms": 500, "cap_ms": 100},
        });
        let error = validate(&backwards).expect_err("seed above cap");
        assert!(error.message().contains("seed_ms"), "{}", error.message());
    }

    fn request(model: &str) -> GenerateRequest {
        GenerateRequest {
            model: model.to_owned(),
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
                tool_choice: ToolChoice::Auto,
                ..TextIntent::default()
            },
        }
    }

    struct Recorder {
        body: serde_json::Value,
        calls: std::sync::Mutex<usize>,
    }

    #[async_trait::async_trait]
    impl Transport for Recorder {
        async fn post_json(
            &self,
            _url: &str,
            _headers: &[(&str, &str)],
            _body: &serde_json::Value,
        ) -> Result<serde_json::Value> {
            *self.calls.lock().expect("lock") += 1;
            Ok(self.body.clone())
        }
    }

    fn provider(transport: Arc<dyn Transport>) -> Local {
        let settings: Settings = serde_json::from_value(settings(json!([
            {"name": "gpt-5", "base_url": "https://api.openai.com", "api_key": "k",
             "max_output_tokens": 64},
        ])))
        .expect("settings");
        let mut models = BTreeMap::new();
        for model in &settings.models {
            models.insert(
                model.name.clone(),
                Model {
                    base_url: model.base_url.clone(),
                    api_key: model.api_key.clone(),
                    max_output_tokens: model.max_output_tokens,
                    timeout: Duration::from_secs(model.timeout_secs),
                },
            );
        }
        Local::with_transport(models, transport)
    }

    /// The catalogue is the deployment's, not the adapter's: a platform that guesses which
    /// models exist will route to one the operator never bought.
    #[tokio::test]
    async fn the_catalogue_comes_from_the_settings() {
        let local = provider(Arc::new(Recorder {
            body: json!({}),
            calls: std::sync::Mutex::new(0),
        }));
        let models = local.models().await.expect("models");
        assert_eq!(models.len(), 1);
        assert_eq!(models[0].name, "gpt-5");
        let capabilities = local.capabilities().await.expect("capabilities");
        assert_eq!(capabilities.models, 1);
        assert_eq!(capabilities.operations, vec![Operation::Generate]);
        assert!(capabilities.streams);
    }

    /// `Explain` compiles and stops: it must not spend a call.
    #[tokio::test]
    async fn explain_compiles_without_calling_the_provider() {
        let transport = Arc::new(Recorder {
            body: json!({}),
            calls: std::sync::Mutex::new(0),
        });
        let local = provider(transport.clone());
        let report = local.explain(&request("gpt-5")).await.expect("report");
        // The deployment's ceiling is in the report because it is in the compile.
        assert!(report.decisions.iter().any(|decision| {
            decision.field == crate::inference::model::FieldId::GenerateIntentTextMaxOutputTokens
                && decision.disposition == crate::inference::model::Disposition::Native
        }));
        assert!(!report.rejects(crate::inference::model::FieldId::GenerateContextImage));
        assert_eq!(
            *transport.calls.lock().expect("lock"),
            0,
            "no call was made"
        );
    }

    #[tokio::test]
    async fn generate_calls_once_and_returns_the_answer_with_its_evidence() {
        let transport = Arc::new(Recorder {
            body: json!({
                "output": [{"type": "message", "content": [{"type": "output_text", "text": "hello"}]}],
                "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
            }),
            calls: std::sync::Mutex::new(0),
        });
        let local = provider(transport.clone());
        let generation = local.generate(request("gpt-5")).await.expect("generation");
        assert_eq!(*transport.calls.lock().expect("lock"), 1);
        assert_eq!(generation.response.usage.total_tokens, 4);
        // The report covers the effective request: the deployment's ceiling (64) is in it.
        let mut effective = request("gpt-5");
        effective.intent.max_output_tokens = Some(64);
        generation
            .report
            .validate_success(Operation::Generate, &active_fields(&effective))
            .expect("a complete report");
    }

    /// An undeclared model is `NotFound`, not a guess and not a call.
    #[tokio::test]
    async fn an_undeclared_model_is_not_found() {
        let local = provider(Arc::new(Recorder {
            body: json!({}),
            calls: std::sync::Mutex::new(0),
        }));
        let error = local
            .generate(request("gpt-6"))
            .await
            .expect_err("undeclared");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotFound);
    }

    /// The deployment's ceiling fills a request that asks for none, and a request that
    /// asks for its own keeps it.
    #[tokio::test]
    async fn the_deployments_ceiling_applies_only_where_the_request_has_none() {
        let settings: Settings = serde_json::from_value(settings(json!([
            {"name": "gpt-5", "base_url": "https://api.openai.com", "max_output_tokens": 64},
        ])))
        .expect("settings");
        let transport = Arc::new(Recorder {
            body: json!({"output": [], "usage": {}}),
            calls: std::sync::Mutex::new(0),
        });
        let mut models = BTreeMap::new();
        for model in &settings.models {
            models.insert(
                model.name.clone(),
                Model {
                    base_url: model.base_url.clone(),
                    api_key: String::new(),
                    max_output_tokens: model.max_output_tokens,
                    timeout: Duration::from_secs(model.timeout_secs),
                },
            );
        }
        let local = Local::with_transport(models, transport);
        // The compiled body is what carries the ceiling, so ask the compiler.
        let model = local.model("gpt-5").expect("model");
        let plain = request("gpt-5");
        let applied = local.intent_for(model, &plain);
        assert_eq!(applied.max_output_tokens, Some(64));
        let mut asked = request("gpt-5");
        asked.intent.max_output_tokens = Some(8);
        let kept = local.intent_for(model, &asked);
        assert_eq!(kept.max_output_tokens, Some(8));
    }

    #[test]
    fn a_deployment_that_names_a_dialect_this_build_lacks_is_refused() {
        let error = validate(&settings(json!([
            {"name": "gpt-5", "base_url": "https://api.openai.com", "api": "chat"},
        ])))
        .expect_err("refused");
        assert!(error.to_string().contains("only response is"), "{error}");

        let error = validate(&settings(json!([
            {"name": "gpt-5", "base_url": "https://api.openai.com"},
            {"name": "gpt-5", "base_url": "https://api.openai.com"},
        ])))
        .expect_err("duplicate");
        assert!(error.to_string().contains("declared twice"), "{error}");

        assert!(
            validate(&settings(json!([
                {"name": "gpt-5", "base_url": "https://api.openai.com"},
            ])))
            .is_ok()
        );
    }
}
