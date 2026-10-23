//! The endpoint server: a runner exposed over gRPC.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

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
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::contract::{
    self, CONTRACT, Capabilities, ExecOptions, ExitStatus, Handle, NetPolicy, ResourceLimits,
    Session, SessionFeatures, SessionSpec, WriteMode,
};
use crate::scope::Scope;

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
                sessions: Mutex::new(BTreeMap::new()),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
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
    sessions: Mutex<BTreeMap<String, Arc<dyn Session>>>,
    descriptor: EndpointDescriptor,
    capability: String,
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

    async fn session(&self, id: &str) -> std::result::Result<Arc<dyn Session>, Status> {
        let sessions = self.sessions.lock().await;
        sessions
            .get(id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("sandbox: session {id:?}")))
    }
}

fn parse_duration(raw: &str) -> Result<Option<Duration>> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Ok(None);
    }
    let split = raw.find(|c: char| !c.is_ascii_digit()).unwrap_or(raw.len());
    let (number, unit) = raw.split_at(split);
    let value: u64 = number
        .parse()
        .map_err(|_| Error::validation(format!("timeout {raw:?} is not a duration")))?;
    let duration = match unit {
        "" | "s" => Duration::from_secs(value),
        "ms" => Duration::from_millis(value),
        "m" => Duration::from_secs(value.saturating_mul(60)),
        "h" => Duration::from_secs(value.saturating_mul(3600)),
        other => {
            return Err(Error::validation(format!(
                "timeout {raw:?}: unknown unit {other:?}"
            )));
        }
    };
    Ok(Some(duration))
}

fn runner_features_to_proto(features: SessionFeatures) -> pb::Features {
    pb::Features {
        tty: features.tty,
        signal: features.signal,
        events: features.events,
    }
}

fn features_to_proto(features: SessionFeatures) -> pb::SessionFeatures {
    pb::SessionFeatures {
        tty: features.tty,
        stdin: features.stdin,
        signal: features.signal,
        events: features.events,
    }
}

fn exit_to_proto(exit: &ExitStatus) -> pb::SessionExit {
    pb::SessionExit {
        code: exit.code.unwrap_or(-1),
        signal: exit.signal.unwrap_or(0),
        reason: exit.reason.clone(),
    }
}

fn capabilities_to_proto(capabilities: Capabilities) -> pb::DescribeResponse {
    pb::DescribeResponse {
        policy: Some(pb::Enforcement {
            env_allow_list: capabilities.policy.env_allow_list,
            net_modes: capabilities.policy.net_modes,
            socks5: false,
            mitm: false,
            unix_socket_policy: false,
            memory_cap: capabilities.policy.memory_cap,
            cpu_cap: capabilities.policy.cpu_cap,
            disk_cap: capabilities.policy.disk_cap,
            filesystem_bounds: capabilities.policy.filesystem_bounds,
            write_modes: capabilities.policy.write_modes,
        }),
        features: Some(runner_features_to_proto(capabilities.features)),
        journal: None,
    }
}

fn spec_from_proto(spec: pb::SessionSpec) -> Result<SessionSpec> {
    let exec = spec.opts.unwrap_or_default();
    let write = match exec.write.as_str() {
        "" | "default" => WriteMode::Default,
        "read_only" | "read-only" => WriteMode::ReadOnly,
        other => {
            return Err(Error::validation(format!(
                "sandbox: unknown write mode {other:?}"
            )));
        }
    };
    let limits = exec.resources.unwrap_or_default();
    let net = exec.net.unwrap_or_default();
    let env = exec.env.unwrap_or_default();
    Ok(SessionSpec {
        id: spec.id,
        argv: spec.argv,
        tty: spec.tty,
        stdin: spec.stdin,
        opts: ExecOptions {
            workdir: Some(exec.workdir).filter(|dir| !dir.is_empty()),
            stdin: Some(exec.stdin).filter(|bytes| !bytes.is_empty()),
            timeout: parse_duration(&exec.timeout)?,
            env: contract::EnvPolicy {
                allow: env.allow,
                inject: env.inject.into_iter().collect(),
            },
            net: NetPolicy { mode: net.mode },
            write,
            limits: ResourceLimits {
                cpu_millicores: u32::try_from(limits.cpu_millicores)
                    .ok()
                    .filter(|value| *value > 0),
                memory_bytes: u64::try_from(limits.memory_bytes)
                    .ok()
                    .filter(|value| *value > 0),
                disk_bytes: u64::try_from(limits.disk_bytes)
                    .ok()
                    .filter(|value| *value > 0),
                max_output_bytes: u64::try_from(limits.max_output_bytes)
                    .ok()
                    .filter(|value| *value > 0),
            },
        },
    })
}

#[tonic::async_trait]
impl sandbox_server::Sandbox for Service {
    async fn describe(
        &self,
        _request: Request<pb::DescribeRequest>,
    ) -> std::result::Result<Response<pb::DescribeResponse>, Status> {
        let capabilities = self.runner.capabilities().await.map_err(status)?;
        Ok(Response::new(capabilities_to_proto(capabilities)))
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
        let mut spec = spec_from_proto(raw).map_err(status)?;
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
            capabilities: Some(features_to_proto(session.features())),
        };
        self.sessions
            .lock()
            .await
            .insert(session.id().to_owned(), session);
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
        Ok(Response::new(pb::ReadResponse {
            next_seq: i64::try_from(read.next_seq).unwrap_or(i64::MAX),
            chunks: read
                .chunks
                .into_iter()
                .map(|chunk| pb::Chunk {
                    seq: i64::try_from(chunk.seq).unwrap_or(i64::MAX),
                    stream: chunk.stream,
                    data: chunk.data,
                })
                .collect(),
            eof: read.eof,
        }))
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

    async fn wait(
        &self,
        request: Request<pb::WaitRequest>,
    ) -> std::result::Result<Response<pb::WaitResponse>, Status> {
        self.scope(&request)?;
        let session = self.session(&request.get_ref().session).await?;
        let exit = session.wait().await.map_err(status)?;
        Ok(Response::new(pb::WaitResponse {
            exit: Some(exit_to_proto(&exit)),
        }))
    }

    async fn close_session(
        &self,
        request: Request<pb::CloseSessionRequest>,
    ) -> std::result::Result<Response<pb::CloseSessionResponse>, Status> {
        self.scope(&request)?;
        let id = request.get_ref().session.clone();
        let session = self.sessions.lock().await.remove(&id);
        if let Some(session) = session {
            session.terminate().await.map_err(status)?;
        }
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
            sessions: sessions
                .into_iter()
                .map(|info| pb::SessionInfo {
                    id: info.id,
                    argv: info.argv,
                    tty: info.features.tty,
                    pid: info
                        .pid
                        .and_then(|pid| i32::try_from(pid).ok())
                        .unwrap_or(0),
                    started_at_unix_nano: i64::try_from(info.started_at_unix_nano)
                        .unwrap_or(i64::MAX),
                    running: info.running,
                    exit: info.exit.as_ref().map(exit_to_proto),
                })
                .collect(),
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
        self.sessions.lock().await.clear();
        Ok(Response::new(pb::CloseRunnerResponse {}))
    }

    // The v1 wire keeps these local-only; the proto says so explicitly. A
    // caller that needs them must run in-process or wait for v2.
    async fn resize(
        &self,
        _request: Request<pb::ResizeRequest>,
    ) -> std::result::Result<Response<pb::ResizeResponse>, Status> {
        Err(Status::unimplemented("sandbox: resize is local-only in v1"))
    }

    async fn signal(
        &self,
        _request: Request<pb::SignalRequest>,
    ) -> std::result::Result<Response<pb::SignalResponse>, Status> {
        Err(Status::unimplemented("sandbox: signal is local-only in v1"))
    }

    type WatchStream = std::pin::Pin<
        Box<
            dyn tokio_stream::Stream<Item = std::result::Result<pb::Event, Status>>
                + Send
                + 'static,
        >,
    >;

    async fn watch(
        &self,
        _request: Request<pb::WatchRequest>,
    ) -> std::result::Result<Response<Self::WatchStream>, Status> {
        Err(Status::unimplemented("sandbox: watch is local-only in v1"))
    }

    async fn open_journal(
        &self,
        _request: Request<pb::OpenJournalRequest>,
    ) -> std::result::Result<Response<pb::OpenJournalResponse>, Status> {
        Err(Status::unimplemented(
            "sandbox: journal is not implemented yet",
        ))
    }

    async fn journal_read(
        &self,
        _request: Request<pb::JournalReadRequest>,
    ) -> std::result::Result<Response<pb::JournalReadResponse>, Status> {
        Err(Status::unimplemented(
            "sandbox: journal is not implemented yet",
        ))
    }

    async fn close_journal(
        &self,
        _request: Request<pb::CloseJournalRequest>,
    ) -> std::result::Result<Response<pb::CloseJournalResponse>, Status> {
        Err(Status::unimplemented(
            "sandbox: journal is not implemented yet",
        ))
    }
}
