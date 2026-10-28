//! The endpoint server: a tool source exposed over gRPC.
//!
//! The service validates the catalog it serves — names, schemas, duplicates —
//! and returns it in name order, so a peer never receives a definition this
//! endpoint would itself refuse. `Execute` validates the arguments before it
//! charges the call budget: a malformed request is a caller error, not provider
//! capacity.

use std::collections::{BTreeSet, HashMap};
use std::sync::{Arc, Mutex};

use crate::client::grpc::{authenticate, grant_of, status};
use crate::error::{Error, Result};
use crate::kernel::handles::contracts;
use crate::kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grant, Grants};
use crate::proto::tool::{
    ExecuteRequest, ExecuteResponse, ListRequest, ListResponse, ToolDescriptor,
    tool_source_server::{self, ToolSourceServer},
};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};
use tracing::Instrument;

use crate::caps::tool::contract::{CONTRACT, Handle};
use crate::caps::tool::scope::Scope;
use crate::message::{validate_arguments, validate_content, validate_definition};

/// The registry kind.
pub const KIND: &str = "tool.Endpoint";
/// The implementation name.
pub const IMPL: &str = "grpc";
/// The capability this endpoint serves by default.
pub const CAPABILITY: &str = "tool.ToolSource/v1";

/// The resource's own contract identity.
pub const CONTRACT_ENDPOINT: &str = "tool.Endpoint/v1";

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    capability: Option<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Ok(())
}

/// A resource that serves a tool source on the endpoint named in its settings.
pub struct Endpoint {
    source: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: Mutex<Option<crate::kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("tool.Endpoint")
            .field("endpoint", &self.descriptor.name)
            .field("capability", &self.capability)
            .finish_non_exhaustive()
    }
}

impl Resource for Endpoint {
    fn start(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            let (mut server, listener) = self.host.bind(&self.descriptor.name)?;
            let service = Service {
                source: self.source.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
                budget: Budget::default(),
            };
            let interceptor = GrantInterceptor {
                grants: self.grants.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let router =
                server.add_service(ToolSourceServer::with_interceptor(service, interceptor));
            let mut serving = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?;
            *serving = Some(crate::kernel::Serving::spawn(router, listener));
            Ok(())
        })
    }

    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            let handle = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?
                .take();
            if let Some(serving) = handle {
                serving.shutdown().await?;
            }
            Ok(())
        })
    }
}

/// The `tool.Endpoint/grpc` factory.
pub struct EndpointFactory;

impl Factory for EndpointFactory {
    type Output = Endpoint;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut source = require::<Handle>("source");
        source.contract = Some(CONTRACT);
        let mut host = require::<EndpointHost>("host");
        host.contract = Some(contracts::ENDPOINT_HOST);
        let mut grants = require::<Grants>("grants");
        grants.contract = Some(contracts::GRANTS);
        Spec {
            contract: Some(CONTRACT_ENDPOINT),
            imp: Some(IMPL),
            deps: vec![source, host, grants],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let host = ctx.deps.get::<EndpointHost>("host")?;
            let grants = ctx.deps.get::<Grants>("grants")?;
            let source = ctx.deps.get::<Handle>("source")?;

            let descriptor = host.catalog().resolve(&settings.endpoint)?;
            let capability = settings.capability.unwrap_or_else(|| CONTRACT.to_owned());
            if !descriptor.supports(&capability) {
                return Err(Error::not_available(format!(
                    "endpoint {:?} does not serve {capability:?}",
                    descriptor.name
                )));
            }

            Ok(Arc::new(Endpoint {
                source: Handle::new(source.source()),
                host: (*host).clone(),
                grants: (*grants).clone(),
                descriptor,
                capability,
                serving: Mutex::new(None),
            }))
        })
    }
}

#[derive(Clone)]
struct GrantInterceptor {
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
}

impl Interceptor for GrantInterceptor {
    fn call(&mut self, mut request: Request<()>) -> std::result::Result<Request<()>, Status> {
        let grant = authenticate(&request, &self.grants, &self.descriptor, &self.capability)?;
        request.extensions_mut().insert(grant);
        Ok(request)
    }
}

struct Service {
    source: Handle,
    descriptor: EndpointDescriptor,
    capability: String,
    budget: Budget,
}

/// Call budgets, keyed by the authorization they belong to.
///
/// A lease renews by minting a fresh token, but the session is what the caller
/// was authorized for, so the key leaves the token out: renewing must not
/// refill the budget.
#[derive(Default)]
struct Budget {
    calls: Mutex<HashMap<BudgetKey, u64>>,
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
struct BudgetKey {
    session: String,
    endpoint: String,
    capability: String,
}

impl Budget {
    fn charge(&self, grant: &Grant, max: u64) -> Result<()> {
        let key = BudgetKey {
            session: grant.session.clone(),
            endpoint: grant.endpoint.clone(),
            capability: grant.capability.clone(),
        };
        let mut calls = self
            .calls
            .lock()
            .map_err(|_| Error::internal("tool budget lock poisoned"))?;
        let used = calls.entry(key).or_insert(0);
        if *used >= max {
            return Err(Error::rate_limit(format!(
                "tool lease exhausted its {max} calls"
            )));
        }
        *used += 1;
        Ok(())
    }
}

impl Service {
    fn scope<T>(&self, request: &Request<T>) -> std::result::Result<Scope, Status> {
        let grant = grant_of(request)?;
        self.descriptor.authorize(&grant).map_err(status)?;
        if grant.capability != self.capability {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        Scope::parse(&grant.scope).map_err(|_| Status::permission_denied("grant scope is invalid"))
    }

    /// Charge one call against the lease's budget.
    fn charge(&self, grant: &Grant, scope: &Scope) -> std::result::Result<(), Status> {
        match scope.max_calls {
            Some(max) => self.budget.charge(grant, max).map_err(status),
            None => Ok(()),
        }
    }
}

#[tonic::async_trait]
impl tool_source_server::ToolSource for Service {
    async fn list(
        &self,
        request: Request<ListRequest>,
    ) -> std::result::Result<Response<ListResponse>, Status> {
        let span = tracing::info_span!(
            "tool.endpoint.list",
            endpoint = %self.descriptor.name,
            capability = %self.capability,
        );
        async move {
            let scope = self.scope(&request)?;
            let mut definitions = self.source.list().await.map_err(status)?;
            // A source is code, and code can be wrong. Validate what this
            // endpoint is about to hand out once, here, rather than leaving the
            // first complaint to a client that cannot fix it.
            {
                let mut seen = BTreeSet::new();
                for definition in &definitions {
                    validate_definition(definition).map_err(|error| {
                        status(Error::internal(format!(
                            "tool source at endpoint {:?} returned an invalid definition: {error}",
                            self.descriptor.name
                        )))
                    })?;
                    if !seen.insert(definition.name.as_str()) {
                        return Err(status(Error::internal(format!(
                            "tool source at endpoint {:?} returned {:?} twice",
                            self.descriptor.name, definition.name
                        ))));
                    }
                }
            }
            definitions.sort_by(|left, right| left.name.cmp(&right.name));
            let tools = scope
                .filter(definitions)
                .into_iter()
                .map(|definition| ToolDescriptor {
                    name: definition.name.clone(),
                    definition: Some(definition),
                })
                .collect();
            Ok(Response::new(ListResponse { tools }))
        }
        .instrument(span)
        .await
    }

    async fn execute(
        &self,
        request: Request<ExecuteRequest>,
    ) -> std::result::Result<Response<ExecuteResponse>, Status> {
        let span = tracing::info_span!(
            "tool.endpoint.execute",
            endpoint = %self.descriptor.name,
            capability = %self.capability,
            tool = tracing::field::Empty,
        );
        let record = span.clone();
        async move {
            let scope = self.scope(&request)?;
            let grant = grant_of(&request)?;
            let request = request.into_inner();
            record.record("tool", request.name.as_str());
            scope.allows(&request.name).map_err(status)?;
            // Validate before charging: a malformed call must never spend the
            // lease's budget or reach the provider.
            validate_arguments(&request.arguments).map_err(status)?;
            self.charge(&grant, &scope)?;
            let content = self
                .source
                .execute(&request.name, &request.arguments)
                .await
                .map_err(status)?;
            validate_content(&content).map_err(status)?;
            Ok(Response::new(ExecuteResponse {
                content: Some(content),
            }))
        }
        .instrument(span)
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use crate::caps::tool::contract::ToolSource;
    use crate::message::text_content;
    use crate::proto::message as msg;
    use crate::proto::tool::tool_source_server::ToolSource as _;

    fn grant(session: &str, token: &str) -> Grant {
        grant_with(session, token, serde_json::json!({}))
    }

    fn grant_with(session: &str, token: &str, scope: Value) -> Grant {
        Grant {
            id: token.to_owned(),
            session: session.to_owned(),
            tenant: "t".to_owned(),
            capability: CAPABILITY.to_owned(),
            endpoint: "tools".to_owned(),
            scope,
            issued_at: 0,
            expires_at: 0,
            token: token.to_owned(),
        }
    }

    fn service(source: Arc<dyn ToolSource>) -> Service {
        Service {
            source: Handle::new(source),
            descriptor: EndpointDescriptor {
                name: "tools".to_owned(),
                target: "unix:///tmp/tools.sock".to_owned(),
                tenant: String::new(),
                capabilities: vec![CAPABILITY.to_owned()],
                tls: None,
            },
            capability: CAPABILITY.to_owned(),
            budget: Budget::default(),
        }
    }

    fn request<T>(payload: T, grant: Grant) -> Request<T> {
        let mut request = Request::new(payload);
        request.extensions_mut().insert(grant);
        request
    }

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    /// A source with a fixed catalog; `calls` counts executions.
    struct Static {
        definitions: Vec<msg::ToolDefinition>,
        calls: Arc<AtomicUsize>,
    }

    #[async_trait::async_trait]
    impl ToolSource for Static {
        async fn list(&self) -> Result<Vec<msg::ToolDefinition>> {
            Ok(self.definitions.clone())
        }

        async fn execute(&self, _name: &str, arguments: &str) -> Result<msg::Content> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(text_content(arguments))
        }
    }

    fn static_source(
        definitions: Vec<msg::ToolDefinition>,
    ) -> (Arc<dyn ToolSource>, Arc<AtomicUsize>) {
        let calls = Arc::new(AtomicUsize::new(0));
        let source = Arc::new(Static {
            definitions,
            calls: calls.clone(),
        });
        (source as Arc<dyn ToolSource>, calls)
    }

    #[tokio::test]
    async fn list_is_sorted_and_each_definition_matches_its_name() {
        let (source, _calls) = static_source(vec![definition("zeta"), definition("alpha")]);
        let response = service(source)
            .list(request(ListRequest {}, grant("s1", "t1")))
            .await
            .expect("list")
            .into_inner();
        let names: Vec<&str> = response
            .tools
            .iter()
            .map(|tool| tool.name.as_str())
            .collect();
        assert_eq!(names, vec!["alpha", "zeta"]);
        for tool in &response.tools {
            let definition = tool.definition.as_ref().expect("definition");
            assert_eq!(tool.name, definition.name);
        }
    }

    #[tokio::test]
    async fn a_source_that_lists_a_name_twice_is_refused() {
        let (source, _calls) = static_source(vec![definition("echo"), definition("echo")]);
        let error = service(source)
            .list(request(ListRequest {}, grant("s1", "t1")))
            .await
            .expect_err("duplicate");
        assert_eq!(error.code(), tonic::Code::Internal);
        assert!(error.message().contains("twice"), "{}", error.message());
    }

    #[tokio::test]
    async fn a_source_with_an_invalid_definition_is_refused() {
        let (source, _calls) = static_source(vec![definition("two words")]);
        let error = service(source)
            .list(request(ListRequest {}, grant("s1", "t1")))
            .await
            .expect_err("invalid");
        assert_eq!(error.code(), tonic::Code::Internal);
        assert!(
            error.message().contains("invalid definition"),
            "{}",
            error.message()
        );
    }

    #[tokio::test]
    async fn a_malformed_call_is_refused_before_it_spends_the_budget() {
        let (source, calls) = static_source(vec![definition("echo")]);
        let service = service(source);
        let scope = serde_json::json!({"allow": ["echo"], "max_calls": 1});

        let error = service
            .execute(request(
                ExecuteRequest {
                    name: "echo".to_owned(),
                    arguments: "[1]".to_owned(),
                },
                grant_with("s1", "t1", scope.clone()),
            ))
            .await
            .expect_err("array arguments");
        assert_eq!(error.code(), tonic::Code::InvalidArgument);
        assert_eq!(calls.load(Ordering::SeqCst), 0, "the provider must not run");

        service
            .execute(request(
                ExecuteRequest {
                    name: "echo".to_owned(),
                    arguments: "{}".to_owned(),
                },
                grant_with("s1", "t1", scope.clone()),
            ))
            .await
            .expect("the malformed call spent no budget");
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        let error = service
            .execute(request(
                ExecuteRequest {
                    name: "echo".to_owned(),
                    arguments: "{}".to_owned(),
                },
                grant_with("s1", "t1", scope),
            ))
            .await
            .expect_err("exhausted");
        assert_eq!(error.code(), tonic::Code::ResourceExhausted);
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn a_renewed_lease_shares_the_budget_of_its_session() {
        let budget = Budget::default();
        budget.charge(&grant("s1", "t1"), 2).expect("first");
        budget.charge(&grant("s1", "t1"), 2).expect("second");
        // A renewal mints a new token (and jti) for the same session.
        let error = budget.charge(&grant("s1", "t2"), 2).expect_err("exhausted");
        assert_eq!(error.kind(), crate::error::ErrorKind::RateLimit);
        // Another session has its own budget.
        budget.charge(&grant("s2", "t3"), 2).expect("other session");
    }
}
