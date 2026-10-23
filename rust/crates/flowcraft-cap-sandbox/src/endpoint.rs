//! The endpoint server: a runner exposed over gRPC.
//!
//! The service holds no session table of its own: every call resolves the
//! session through the runner, so there is exactly one answer to "is this
//! session open" and a session started by one call is visible to the next
//! without any bookkeeping to keep in sync.

use std::collections::BTreeMap;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use flowcraft_client::grpc::{authenticate, grant_of, status};
use flowcraft_error::{Error, Result};
use flowcraft_kernel::handles::contracts;
use flowcraft_kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use flowcraft_proto::sandbox as pb;
use flowcraft_proto::sandbox::sandbox_server;
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tokio::sync::Mutex;
use tokio_stream::{Stream, StreamExt};
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::contract::{self, CONTRACT, Capabilities, Handle};
use crate::journal::Journal;
use crate::scope::Scope;
use crate::wire;

/// The registry kind.
pub const KIND: &str = "sandbox.Endpoint";
/// The implementation name.
pub const IMPL: &str = "grpc";
/// The capability this endpoint serves by default.
pub const CAPABILITY: &str = "sandbox.Runner/v1";

/// The resource's own contract identity.
pub const CONTRACT_ENDPOINT: &str = "sandbox.Endpoint/v1";

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    capability: Option<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Ok(())
}

/// A resource that serves a runner on the endpoint named in its settings.
pub struct Endpoint {
    runner: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: std::sync::Mutex<Option<flowcraft_kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Endpoint")
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
                runner: self.runner.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
                journals: Mutex::new(BTreeMap::new()),
                next_journal: AtomicU64::new(1),
            };
            let interceptor = GrantInterceptor {
                grants: self.grants.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let router = server.add_service(sandbox_server::SandboxServer::with_interceptor(
                service,
                interceptor,
            ));
            let mut serving = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?;
            *serving = Some(flowcraft_kernel::Serving::spawn(router, listener));
            Ok(())
        })
    }

    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            self.runner.close().await?;
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

/// The `sandbox.Endpoint/grpc` factory.
pub struct EndpointFactory;

impl Factory for EndpointFactory {
    type Output = Endpoint;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut runner = require::<Handle>("runner");
        runner.contract = Some(CONTRACT);
        let mut host = require::<EndpointHost>("host");
        host.contract = Some(contracts::ENDPOINT_HOST);
        let mut grants = require::<Grants>("grants");
        grants.contract = Some(contracts::GRANTS);
        Spec {
            contract: Some(CONTRACT_ENDPOINT),
            imp: Some(IMPL),
            deps: vec![runner, host, grants],
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
            let runner = ctx.deps.get::<Handle>("runner")?;

            let descriptor = host.catalog().resolve(&settings.endpoint)?;
            let capability = settings.capability.unwrap_or_else(|| CONTRACT.to_owned());
            if !descriptor.supports(&capability) {
                return Err(Error::not_available(format!(
                    "endpoint {:?} does not serve {capability:?}",
                    descriptor.name
                )));
            }

            Ok(Arc::new(Endpoint {
                runner: Handle::new(runner.runner()),
                host: (*host).clone(),
                grants: (*grants).clone(),
                descriptor,
                capability,
                serving: std::sync::Mutex::new(None),
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
    runner: Handle,
    descriptor: EndpointDescriptor,
    capability: String,
    /// Open journal readers, by the id handed out on the wire.
    ///
    /// This is the one table the endpoint legitimately owns: a journal handle is
    /// *addressed* by an opaque id, and the runner has no way to look one up
    /// again.
    journals: Mutex<BTreeMap<String, Arc<dyn Journal>>>,
    next_journal: AtomicU64,
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

    async fn session(
        &self,
        id: &str,
    ) -> std::result::Result<Arc<dyn crate::contract::Session>, Status> {
        self.runner.session(id).await.map_err(status)
    }
}

#[tonic::async_trait]
impl sandbox_server::Sandbox for Service {
    async fn describe(
        &self,
        _request: Request<pb::DescribeRequest>,
    ) -> std::result::Result<Response<pb::DescribeResponse>, Status> {
        let capabilities: Capabilities = self.runner.capabilities().await.map_err(status)?;
        Ok(Response::new(wire::capabilities_to_proto(&capabilities)))
    }

    async fn start(
        &self,
        request: Request<pb::StartRequest>,
    ) -> std::result::Result<Response<pb::StartResponse>, Status> {
        let scope = self.scope(&request)?;
        let raw = request
            .get_ref()
            .spec
            .clone()
            .ok_or_else(|| Status::invalid_argument("sandbox: spec is required"))?;
        let mut spec = wire::spec_from_proto(raw).map_err(status)?;
        scope.allows_command(&spec.argv).map_err(status)?;
        spec.opts.timeout = scope.timeout(spec.opts.timeout).map_err(status)?;
        spec.opts.limits.max_output_bytes = scope.output_limit(spec.opts.limits.max_output_bytes);
        let session = self.runner.start(spec).await.map_err(status)?;
        let reference = pb::SessionRef {
            id: session.id().to_owned(),
            pid: session
                .pid()
                .and_then(|pid| i32::try_from(pid).ok())
                .unwrap_or(0),
            capabilities: Some(wire::features_to_proto(session.features())),
        };
        Ok(Response::new(pb::StartResponse {
            session: Some(reference),
        }))
    }

    async fn read(
        &self,
        request: Request<pb::ReadRequest>,
    ) -> std::result::Result<Response<pb::ReadResponse>, Status> {
        self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&payload.session).await?;
        let after = u64::try_from(payload.after_seq).unwrap_or(0);
        let max = usize::try_from(payload.max_bytes).unwrap_or(usize::MAX);
        let read = session.read(after, max).await.map_err(status)?;
        Ok(Response::new(wire::read_to_proto(&read)))
    }

    async fn write(
        &self,
        request: Request<pb::WriteRequest>,
    ) -> std::result::Result<Response<pb::WriteResponse>, Status> {
        self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&payload.session).await?;
        session.write(&payload.data).await.map_err(status)?;
        Ok(Response::new(pb::WriteResponse {}))
    }

    async fn close_input(
        &self,
        request: Request<pb::CloseInputRequest>,
    ) -> std::result::Result<Response<pb::CloseInputResponse>, Status> {
        self.scope(&request)?;
        let session = self.session(&request.get_ref().session).await?;
        session.close_input().await.map_err(status)?;
        Ok(Response::new(pb::CloseInputResponse {}))
    }

    async fn resize(
        &self,
        request: Request<pb::ResizeRequest>,
    ) -> std::result::Result<Response<pb::ResizeResponse>, Status> {
        self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&payload.session).await?;
        let rows = u16::try_from(payload.rows).unwrap_or(0);
        let cols = u16::try_from(payload.cols).unwrap_or(0);
        session.resize(rows, cols).await.map_err(status)?;
        Ok(Response::new(pb::ResizeResponse {}))
    }

    async fn signal(
        &self,
        request: Request<pb::SignalRequest>,
    ) -> std::result::Result<Response<pb::SignalResponse>, Status> {
        self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&payload.session).await?;
        let signal = wire::signal_from_proto(&payload.signal).map_err(status)?;
        session.signal(signal).await.map_err(status)?;
        Ok(Response::new(pb::SignalResponse {}))
    }

    type WatchStream = Pin<Box<dyn Stream<Item = std::result::Result<pb::Event, Status>> + Send>>;

    async fn watch(
        &self,
        request: Request<pb::WatchRequest>,
    ) -> std::result::Result<Response<Self::WatchStream>, Status> {
        self.scope(&request)?;
        let session = self.session(&request.get_ref().session).await?;
        let events = session.watch().map_err(status)?;
        let streamed = events.map(|event| Ok(wire::event_to_proto(&event)));
        Ok(Response::new(Box::pin(streamed)))
    }

    async fn wait(
        &self,
        request: Request<pb::WaitRequest>,
    ) -> std::result::Result<Response<pb::WaitResponse>, Status> {
        self.scope(&request)?;
        let session = self.session(&request.get_ref().session).await?;
        let exit = session.wait().await.map_err(status)?;
        // A timeout or an exhausted budget is a failure of the call as well as
        // an outcome, so the RPC reports the classification; the outcome itself
        // is still carried when it is a plain exit.
        if let Some(error) = exit.error() {
            return Err(status(error));
        }
        Ok(Response::new(pb::WaitResponse {
            exit: Some(wire::exit_to_proto(&exit)),
        }))
    }

    async fn close_session(
        &self,
        request: Request<pb::CloseSessionRequest>,
    ) -> std::result::Result<Response<pb::CloseSessionResponse>, Status> {
        self.scope(&request)?;
        self.runner
            .close_session(&request.get_ref().session)
            .await
            .map_err(status)?;
        Ok(Response::new(pb::CloseSessionResponse {}))
    }

    async fn terminate_session(
        &self,
        request: Request<pb::TerminateSessionRequest>,
    ) -> std::result::Result<Response<pb::TerminateSessionResponse>, Status> {
        self.scope(&request)?;
        let session = self.session(&request.get_ref().session).await?;
        session.terminate().await.map_err(status)?;
        Ok(Response::new(pb::TerminateSessionResponse {}))
    }

    async fn list(
        &self,
        request: Request<pb::ListRequest>,
    ) -> std::result::Result<Response<pb::ListResponse>, Status> {
        self.scope(&request)?;
        let sessions = self.runner.list().await.map_err(status)?;
        Ok(Response::new(pb::ListResponse {
            sessions: sessions.iter().map(wire::info_to_proto).collect(),
        }))
    }

    async fn terminate(
        &self,
        request: Request<pb::TerminateRequest>,
    ) -> std::result::Result<Response<pb::TerminateResponse>, Status> {
        self.scope(&request)?;
        self.runner
            .terminate(&request.get_ref().session)
            .await
            .map_err(status)?;
        Ok(Response::new(pb::TerminateResponse {}))
    }

    async fn close_runner(
        &self,
        request: Request<pb::CloseRunnerRequest>,
    ) -> std::result::Result<Response<pb::CloseRunnerResponse>, Status> {
        self.scope(&request)?;
        self.runner.close().await.map_err(status)?;
        // Readers handed out over the wire die with the runner they read from.
        let readers: Vec<Arc<dyn Journal>> = {
            let journals = self.journals.lock().await;
            journals.values().cloned().collect()
        };
        self.journals.lock().await.clear();
        for reader in readers {
            let _ = reader.close().await;
        }
        Ok(Response::new(pb::CloseRunnerResponse {}))
    }

    async fn open_journal(
        &self,
        request: Request<pb::OpenJournalRequest>,
    ) -> std::result::Result<Response<pb::OpenJournalResponse>, Status> {
        self.scope(&request)?;
        let journal = self.runner.open_journal().await.map_err(status)?;
        let id = format!(
            "journal-{}",
            self.next_journal.fetch_add(1, Ordering::Relaxed)
        );
        self.journals.lock().await.insert(id.clone(), journal);
        Ok(Response::new(pb::OpenJournalResponse { journal: id }))
    }

    async fn journal_read(
        &self,
        request: Request<pb::JournalReadRequest>,
    ) -> std::result::Result<Response<pb::JournalReadResponse>, Status> {
        self.scope(&request)?;
        let payload = request.get_ref();
        let journal = self.journals.lock().await.get(&payload.journal).cloned();
        let journal = journal
            .ok_or_else(|| Status::not_found(format!("sandbox: journal {:?}", payload.journal)))?;
        let after = u64::try_from(payload.after_seq).unwrap_or(0);
        let max = usize::try_from(payload.max).unwrap_or(usize::MAX);
        let batch = journal.read(after, max).await.map_err(status)?;
        Ok(Response::new(wire::batch_to_proto(&batch)))
    }

    async fn close_journal(
        &self,
        request: Request<pb::CloseJournalRequest>,
    ) -> std::result::Result<Response<pb::CloseJournalResponse>, Status> {
        self.scope(&request)?;
        let journal = self
            .journals
            .lock()
            .await
            .remove(&request.get_ref().journal);
        if let Some(journal) = journal {
            journal.close().await.map_err(status)?;
        }
        Ok(Response::new(pb::CloseJournalResponse {}))
    }
}

/// Keep the contract import used by the capability constant above.
const _: &str = contract::CONTRACT;
