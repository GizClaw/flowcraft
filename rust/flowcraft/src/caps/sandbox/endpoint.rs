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

use crate::client::grpc::{authenticate, grant_of, status};
use crate::error::{Error, Result};
use crate::kernel::handles::contracts;
use crate::kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use crate::proto::sandbox as pb;
use crate::proto::sandbox::sandbox_server;
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tokio::sync::Mutex;
use tokio_stream::{Stream, StreamExt};
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::caps::sandbox::contract::{self, CONTRACT, Capabilities, Handle};
use crate::caps::sandbox::journal::Journal;
use crate::caps::sandbox::policy::plan::plan;
use crate::caps::sandbox::scope::Scope;
use crate::caps::sandbox::wire;

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
    let settings: Settings = crate::resource::settings(value)?;
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
    serving: std::sync::Mutex<Option<crate::kernel::Serving>>,
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
                started: Mutex::new(BTreeMap::new()),
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
            *serving = Some(crate::kernel::Serving::spawn(router, listener));
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

/// One open journal reader, with the session that opened it.
type JournalHandles = Mutex<BTreeMap<String, (String, Arc<dyn Journal>)>>;

struct Service {
    runner: Handle,
    descriptor: EndpointDescriptor,
    capability: String,
    /// Open journal readers, by the id handed out on the wire.
    ///
    /// This is the one table the endpoint legitimately owns: a journal handle is
    /// *addressed* by an opaque id, and the runner has no way to look one up
    /// again.
    journals: JournalHandles,
    next_journal: AtomicU64,
    /// Ids this endpoint started.
    ///
    /// The runner's registry is the source of truth for session *state*, but it
    /// also holds sessions the host started in-process (a one-shot `exec`, a
    /// craft's own session). A grant is not an ownership token: without this
    /// set, any client allowed to call `Read` could read a session it never
    /// started. Membership is the missing dimension, so it is kept here rather
    /// than widening the grant scope.
    started: Mutex<BTreeMap<String, String>>,
}

impl Service {
    fn scope<T>(&self, request: &Request<T>) -> std::result::Result<(String, Scope), Status> {
        let grant = grant_of(request)?;
        self.descriptor.authorize(&grant).map_err(status)?;
        if grant.capability != self.capability {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        let scope = Scope::parse(&grant.scope)
            .map_err(|_| Status::permission_denied("grant scope is invalid"))?;
        // The session a grant was minted for is the ownership dimension the scope
        // does not have: two clients of the same session are one principal, and
        // different sessions must not see each other's handles.
        Ok((grant.session.clone(), scope))
    }

    async fn session(
        &self,
        owner: &str,
        id: &str,
    ) -> std::result::Result<Arc<dyn crate::caps::sandbox::contract::Session>, Status> {
        if !self.owned(owner, id).await {
            // Not this endpoint's session (or another session's): answering
            // `NotFound` keeps a host-started session invisible instead of merely
            // unlisted.
            return Err(Status::not_found(format!("sandbox: session {id:?}")));
        }
        self.runner.session(id).await.map_err(status)
    }

    async fn owned(&self, owner: &str, id: &str) -> bool {
        self.started
            .lock()
            .await
            .get(id)
            .is_some_and(|session| session == owner)
    }

    async fn forget(&self, id: &str) {
        self.started.lock().await.remove(id);
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
        let (owner, scope) = self.scope(&request)?;
        let raw = request
            .get_ref()
            .spec
            .clone()
            .ok_or_else(|| Status::invalid_argument("sandbox: spec is required"))?;
        let mut spec = wire::spec_from_proto(raw).map_err(status)?;
        // The grant judges the command the executor will run, so the plan comes
        // first: `spec.argv[0]` is a spelling the caller controls, and the
        // grant's rules are not about spellings.
        // The runner knows where this deployment's programmes live, and the grant
        // has to judge the same file the runner will exec.
        let planned = plan(&spec, &self.runner.host_policy());
        scope.allows_command(&planned).map_err(status)?;
        spec.opts.timeout = scope.timeout(spec.opts.timeout).map_err(status)?;
        spec.opts.limits.max_output_bytes = scope.output_limit(spec.opts.limits.max_output_bytes);
        let session = self.runner.start(spec).await.map_err(status)?;
        self.started
            .lock()
            .await
            .insert(session.id().to_owned(), owner.clone());
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
        let (owner, _) = self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&owner, &payload.session).await?;
        let after = u64::try_from(payload.after_seq).unwrap_or(0);
        let max = usize::try_from(payload.max_bytes).unwrap_or(usize::MAX);
        let read = session.read(after, max).await.map_err(status)?;
        Ok(Response::new(wire::read_to_proto(&read)))
    }

    async fn write(
        &self,
        request: Request<pb::WriteRequest>,
    ) -> std::result::Result<Response<pb::WriteResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&owner, &payload.session).await?;
        session.write(&payload.data).await.map_err(status)?;
        Ok(Response::new(pb::WriteResponse {}))
    }

    async fn close_input(
        &self,
        request: Request<pb::CloseInputRequest>,
    ) -> std::result::Result<Response<pb::CloseInputResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let session = self.session(&owner, &request.get_ref().session).await?;
        session.close_input().await.map_err(status)?;
        Ok(Response::new(pb::CloseInputResponse {}))
    }

    async fn resize(
        &self,
        request: Request<pb::ResizeRequest>,
    ) -> std::result::Result<Response<pb::ResizeResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&owner, &payload.session).await?;
        let rows = u16::try_from(payload.rows).unwrap_or(0);
        let cols = u16::try_from(payload.cols).unwrap_or(0);
        session.resize(rows, cols).await.map_err(status)?;
        Ok(Response::new(pb::ResizeResponse {}))
    }

    async fn signal(
        &self,
        request: Request<pb::SignalRequest>,
    ) -> std::result::Result<Response<pb::SignalResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let payload = request.get_ref();
        let session = self.session(&owner, &payload.session).await?;
        let signal = wire::signal_from_proto(&payload.signal).map_err(status)?;
        session.signal(signal).await.map_err(status)?;
        Ok(Response::new(pb::SignalResponse {}))
    }

    type WatchStream = Pin<Box<dyn Stream<Item = std::result::Result<pb::Event, Status>> + Send>>;

    async fn watch(
        &self,
        request: Request<pb::WatchRequest>,
    ) -> std::result::Result<Response<Self::WatchStream>, Status> {
        let (owner, _) = self.scope(&request)?;
        let session = self.session(&owner, &request.get_ref().session).await?;
        let events = session.watch().map_err(status)?;
        let streamed = events.map(|event| Ok(wire::event_to_proto(&event)));
        Ok(Response::new(Box::pin(streamed)))
    }

    async fn wait(
        &self,
        request: Request<pb::WaitRequest>,
    ) -> std::result::Result<Response<pb::WaitResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let session = self.session(&owner, &request.get_ref().session).await?;
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
        let (owner, _) = self.scope(&request)?;
        let id = request.get_ref().session.clone();
        if !self.owned(&owner, &id).await {
            // Closing a session this endpoint never started would let any client
            // kill the host's own work.
            return Err(Status::not_found(format!("sandbox: session {id:?}")));
        }
        self.forget(&id).await;
        match self.runner.close_session(&id).await {
            Ok(()) => {}
            // Closing something already gone is a success: the caller's intent
            // is satisfied, and a retried close must not fail.
            Err(error) if error.kind() == crate::error::ErrorKind::NotFound => {}
            Err(error) => return Err(status(error)),
        }
        Ok(Response::new(pb::CloseSessionResponse {}))
    }

    async fn terminate_session(
        &self,
        request: Request<pb::TerminateSessionRequest>,
    ) -> std::result::Result<Response<pb::TerminateSessionResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let session = self.session(&owner, &request.get_ref().session).await?;
        session.terminate().await.map_err(status)?;
        Ok(Response::new(pb::TerminateSessionResponse {}))
    }

    async fn list(
        &self,
        request: Request<pb::ListRequest>,
    ) -> std::result::Result<Response<pb::ListResponse>, Status> {
        // `List` reports the runner's truth, including host-started sessions; the
        // ownership check on every addressed call keeps those unreachable.
        let (_, _) = self.scope(&request)?;
        let sessions = self.runner.list().await.map_err(status)?;
        Ok(Response::new(pb::ListResponse {
            sessions: sessions.iter().map(wire::info_to_proto).collect(),
        }))
    }

    async fn terminate(
        &self,
        request: Request<pb::TerminateRequest>,
    ) -> std::result::Result<Response<pb::TerminateResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let id = request.get_ref().session.clone();
        if !self.owned(&owner, &id).await {
            return Err(Status::not_found(format!("sandbox: session {id:?}")));
        }
        self.runner.terminate(&id).await.map_err(status)?;
        Ok(Response::new(pb::TerminateResponse {}))
    }

    async fn close_runner(
        &self,
        request: Request<pb::CloseRunnerRequest>,
    ) -> std::result::Result<Response<pb::CloseRunnerResponse>, Status> {
        let (_, _) = self.scope(&request)?;
        self.runner.close().await.map_err(status)?;
        // Readers handed out over the wire die with the runner they read from.
        let readers: Vec<Arc<dyn Journal>> = {
            let journals = self.journals.lock().await;
            journals
                .values()
                .map(|(_, journal)| journal.clone())
                .collect()
        };
        self.journals.lock().await.clear();
        self.started.lock().await.clear();
        for reader in readers {
            let _ = reader.close().await;
        }
        Ok(Response::new(pb::CloseRunnerResponse {}))
    }

    async fn open_journal(
        &self,
        request: Request<pb::OpenJournalRequest>,
    ) -> std::result::Result<Response<pb::OpenJournalResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let journal = self.runner.open_journal().await.map_err(status)?;
        let id = format!(
            "journal-{}",
            self.next_journal.fetch_add(1, Ordering::Relaxed)
        );
        self.journals
            .lock()
            .await
            .insert(id.clone(), (owner.clone(), journal));
        Ok(Response::new(pb::OpenJournalResponse { journal: id }))
    }

    async fn journal_read(
        &self,
        request: Request<pb::JournalReadRequest>,
    ) -> std::result::Result<Response<pb::JournalReadResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let payload = request.get_ref();
        let journal = self.journals.lock().await.get(&payload.journal).cloned();
        let journal = match journal {
            Some((session, journal)) if session == owner => journal,
            _ => {
                return Err(Status::not_found(format!(
                    "sandbox: journal {:?}",
                    payload.journal
                )));
            }
        };
        let after = u64::try_from(payload.after_seq).unwrap_or(0);
        let max = usize::try_from(payload.max).unwrap_or(usize::MAX);
        let batch = journal.read(after, max).await.map_err(status)?;
        Ok(Response::new(wire::batch_to_proto(&batch)))
    }

    async fn close_journal(
        &self,
        request: Request<pb::CloseJournalRequest>,
    ) -> std::result::Result<Response<pb::CloseJournalResponse>, Status> {
        let (owner, _) = self.scope(&request)?;
        let removed = self
            .journals
            .lock()
            .await
            .remove(&request.get_ref().journal);
        match removed {
            Some((session, journal)) if session == owner => {
                journal.close().await.map_err(status)?;
            }
            Some((_, journal)) => {
                // Put it back: a foreign reader must not be closeable.
                self.journals
                    .lock()
                    .await
                    .insert(request.get_ref().journal.clone(), (owner.clone(), journal));
                return Err(Status::permission_denied(
                    "sandbox: journal belongs to another session",
                ));
            }
            None => {}
        }
        Ok(Response::new(pb::CloseJournalResponse {}))
    }
}

/// Keep the contract import used by the capability constant above.
const _: &str = contract::CONTRACT;
