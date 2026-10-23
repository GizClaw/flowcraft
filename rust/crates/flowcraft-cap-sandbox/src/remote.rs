//! The remote client: a remote runner presented as the local contract.
//!
//! Capabilities are fetched once at build time and then reported verbatim, so a
//! remote runner declares exactly what its server declared.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use flowcraft_client::grpc::from_status;
use flowcraft_client::wire::Wire;
use flowcraft_error::{Error, Result};
use flowcraft_kernel::{EndpointProvider, Endpoints};
use flowcraft_proto::sandbox as pb;
use flowcraft_proto::sandbox::sandbox_client::SandboxClient;
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tokio::sync::Mutex;

use crate::codes::{self, Code};
use crate::contract::{
    CONTRACT, Capabilities, Chunk, Enforcement, ExitStatus, Handle, KIND, ReadResult, Runner,
    Session, SessionFeatures, SessionInfo, SessionSpec, WriteMode,
};

/// The implementation name.
pub const IMPL: &str = "remote";

/// A runner served by another process.
pub struct RemoteRunner {
    wire: Arc<Wire>,
    capabilities: std::sync::Mutex<Option<Capabilities>>,
    sessions: Mutex<BTreeMap<String, SessionRef>>,
}

#[derive(Clone, Copy)]
struct SessionRef {
    pid: Option<u32>,
    features: SessionFeatures,
}

/// One session of a remote runner.
pub struct RemoteSession {
    wire: Arc<Wire>,
    id: String,
    pid: Option<u32>,
    features: SessionFeatures,
}

impl std::fmt::Debug for RemoteRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteRunner")
            .field("wire", &self.wire)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Debug for RemoteSession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteSession")
            .field("id", &self.id)
            .finish_non_exhaustive()
    }
}

impl RemoteRunner {
    /// Mint a grant and prepare a client. Capabilities are fetched on demand:
    /// build runs before start, so the endpoint may not be listening yet.
    pub async fn connect(
        provider: Arc<dyn EndpointProvider>,
        endpoint: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Self> {
        Ok(Self {
            wire: Arc::new(Wire::open(provider, endpoint, CONTRACT, tenant, scope).await?),
            capabilities: std::sync::Mutex::new(None),
            sessions: Mutex::new(BTreeMap::new()),
        })
    }
}

fn capabilities_from_proto(response: pb::DescribeResponse) -> Capabilities {
    let enforcement = response.policy.unwrap_or_default();
    let features = response.features.unwrap_or_default();
    Capabilities {
        policy: Enforcement {
            env_allow_list: enforcement.env_allow_list,
            net_modes: enforcement.net_modes,
            filesystem_bounds: enforcement.filesystem_bounds,
            write_modes: enforcement.write_modes,
            memory_cap: enforcement.memory_cap,
            cpu_cap: enforcement.cpu_cap,
            disk_cap: enforcement.disk_cap,
        },
        features: SessionFeatures {
            tty: features.tty,
            stdin: false,
            signal: features.signal,
            events: features.events,
        },
    }
}

fn features_from_proto(features: pb::SessionFeatures) -> SessionFeatures {
    SessionFeatures {
        tty: features.tty,
        stdin: features.stdin,
        signal: features.signal,
        events: features.events,
    }
}

fn exit_from_proto(exit: Option<pb::SessionExit>) -> ExitStatus {
    let exit = exit.unwrap_or_default();
    ExitStatus {
        code: if exit.code >= 0 {
            Some(exit.code)
        } else {
            None
        },
        signal: if exit.signal > 0 {
            Some(exit.signal)
        } else {
            None
        },
        reason: exit.reason,
    }
}

fn format_duration(duration: Duration) -> String {
    if duration.subsec_millis() == 0 && duration.as_secs() > 0 {
        format!("{}s", duration.as_secs())
    } else {
        format!("{}ms", duration.as_millis())
    }
}

fn spec_to_proto(spec: &SessionSpec) -> pb::SessionSpec {
    let opts = &spec.opts;
    pb::SessionSpec {
        id: spec.id.clone(),
        argv: spec.argv.clone(),
        tty: spec.tty,
        rows: 0,
        cols: 0,
        stdin: spec.stdin,
        opts: Some(pb::ExecOptions {
            workdir: opts.workdir.clone().unwrap_or_default(),
            stdin: opts.stdin.clone().unwrap_or_default(),
            timeout: opts.timeout.map(format_duration).unwrap_or_default(),
            env: Some(pb::EnvPolicy {
                allow: opts.env.allow.clone(),
                inject: opts.env.inject.clone().into_iter().collect(),
            }),
            net: Some(pb::NetPolicy {
                mode: opts.net.mode.clone(),
                allow_hosts: Vec::new(),
                rules: Vec::new(),
                proxy: String::new(),
                unix_sockets: Vec::new(),
            }),
            write: match opts.write {
                WriteMode::Default => String::new(),
                WriteMode::ReadOnly => "read_only".to_owned(),
            },
            resources: Some(pb::ResourceLimits {
                cpu_millicores: opts
                    .limits
                    .cpu_millicores
                    .and_then(|value| i32::try_from(value).ok())
                    .unwrap_or(0),
                memory_bytes: opts
                    .limits
                    .memory_bytes
                    .and_then(|value| i64::try_from(value).ok())
                    .unwrap_or(0),
                disk_bytes: opts
                    .limits
                    .disk_bytes
                    .and_then(|value| i64::try_from(value).ok())
                    .unwrap_or(0),
                max_output_bytes: opts
                    .limits
                    .max_output_bytes
                    .and_then(|value| i64::try_from(value).ok())
                    .unwrap_or(0),
            }),
        }),
    }
}

#[async_trait]
impl Runner for RemoteRunner {
    fn capabilities(&self) -> flowcraft_resource::BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move {
            if let Ok(guard) = self.capabilities.lock()
                && let Some(capabilities) = guard.clone()
            {
                return Ok(capabilities);
            }
            let mut client = SandboxClient::new(self.wire.channel().await?);
            let described = client
                .describe(self.wire.request(pb::DescribeRequest {}).await?)
                .await
                .map_err(from_status)?
                .into_inner();
            let capabilities = capabilities_from_proto(described);
            if let Ok(mut guard) = self.capabilities.lock() {
                *guard = Some(capabilities.clone());
            }
            Ok(capabilities)
        })
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .start(
                self.wire
                    .request(pb::StartRequest {
                        spec: Some(spec_to_proto(&spec)),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        let reference = response
            .session
            .ok_or_else(|| Error::internal("sandbox: server returned no session"))?;
        let pid = u32::try_from(reference.pid).ok().filter(|pid| *pid > 0);
        let features = reference
            .capabilities
            .map(features_from_proto)
            .unwrap_or_default();
        let session: Arc<dyn Session> = Arc::new(RemoteSession {
            wire: self.wire.clone(),
            id: reference.id.clone(),
            pid,
            features,
        });
        self.sessions
            .lock()
            .await
            .insert(reference.id, SessionRef { pid, features });
        Ok(session)
    }

    async fn session(&self, id: &str) -> Result<Arc<dyn Session>> {
        let sessions = self.sessions.lock().await;
        let reference = sessions.get(id).ok_or_else(|| {
            codes::not_found(Code::SessionNotFound, format!("sandbox: session {id:?}"))
        })?;
        Ok(Arc::new(RemoteSession {
            wire: self.wire.clone(),
            id: id.to_owned(),
            pid: reference.pid,
            features: reference.features,
        }))
    }

    async fn list(&self) -> Result<Vec<SessionInfo>> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .list(self.wire.request(pb::ListRequest {}).await?)
            .await
            .map_err(from_status)?
            .into_inner();
        Ok(response
            .sessions
            .into_iter()
            .map(|info| SessionInfo {
                id: info.id,
                argv: info.argv,
                pid: u32::try_from(info.pid).ok().filter(|pid| *pid > 0),
                started_at_unix_nano: u64::try_from(info.started_at_unix_nano).unwrap_or(0),
                features: SessionFeatures {
                    tty: info.tty,
                    ..SessionFeatures::default()
                },
                running: info.running,
                exit: info.exit.map(|exit| exit_from_proto(Some(exit))),
            })
            .collect())
    }

    async fn terminate(&self, id: &str) -> Result<()> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .terminate(
                self.wire
                    .request(pb::TerminateRequest {
                        session: id.to_owned(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }

    async fn close(&self) -> Result<()> {
        let outcome = match self.wire.channel().await {
            Ok(channel) => {
                let mut client = SandboxClient::new(channel);
                match self.wire.request(pb::CloseRunnerRequest {}).await {
                    Ok(request) => client.close_runner(request).await.map(|_| ()),
                    Err(error) => return Err(error),
                }
            }
            Err(error) => Err(tonic::Status::unavailable(error.to_string())),
        };
        match outcome {
            Ok(()) => {}
            Err(status) => {
                let error = from_status(status);
                // Shutdown closes resources in reverse order, so the endpoint may
                // already be gone or be cancelling in-flight calls. Close is
                // idempotent: either way there is nothing left to close.
                if !matches!(
                    error.kind(),
                    flowcraft_error::ErrorKind::NotAvailable
                        | flowcraft_error::ErrorKind::Interrupted
                        | flowcraft_error::ErrorKind::Aborted
                ) {
                    return Err(error);
                }
                tracing::debug!("sandbox: endpoint already gone during close");
            }
        }
        self.sessions.lock().await.clear();
        Ok(())
    }
}

#[async_trait]
impl Session for RemoteSession {
    fn id(&self) -> &str {
        &self.id
    }

    fn pid(&self) -> Option<u32> {
        self.pid
    }

    fn features(&self) -> SessionFeatures {
        self.features
    }

    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .read(
                self.wire
                    .request(pb::ReadRequest {
                        session: self.id.clone(),
                        after_seq: i64::try_from(after_seq).unwrap_or(i64::MAX),
                        max_bytes: i32::try_from(max_bytes).unwrap_or(i32::MAX),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        Ok(ReadResult {
            next_seq: u64::try_from(response.next_seq).unwrap_or(after_seq),
            chunks: response
                .chunks
                .into_iter()
                .map(|chunk| Chunk {
                    seq: u64::try_from(chunk.seq).unwrap_or(0),
                    stream: chunk.stream,
                    data: chunk.data,
                })
                .collect(),
            eof: response.eof,
        })
    }

    async fn write(&self, data: &[u8]) -> Result<()> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .write(
                self.wire
                    .request(pb::WriteRequest {
                        session: self.id.clone(),
                        data: data.to_vec(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }

    async fn close_input(&self) -> Result<()> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .close_input(
                self.wire
                    .request(pb::CloseInputRequest {
                        session: self.id.clone(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }

    async fn wait(&self) -> Result<ExitStatus> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .wait(
                self.wire
                    .request(pb::WaitRequest {
                        session: self.id.clone(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        Ok(exit_from_proto(response.exit))
    }

    async fn terminate(&self) -> Result<()> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .terminate_session(
                self.wire
                    .request(pb::TerminateSessionRequest {
                        session: self.id.clone(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    tenant: String,
    #[serde(default)]
    scope: Value,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    crate::scope::Scope::parse(&settings.scope)?;
    Ok(())
}

/// The `sandbox.Runner/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(flowcraft_kernel::handles::contracts::ENDPOINT_PROVIDER);
        Spec {
            contract: Some(CONTRACT),
            imp: Some(IMPL),
            deps: vec![endpoints],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let endpoints = ctx.deps.get::<Endpoints>("endpoints")?;
            let scope = if settings.scope.is_null() {
                serde_json::json!({})
            } else {
                settings.scope
            };
            let runner = RemoteRunner::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                scope,
            )
            .await?;
            Ok(Arc::new(Handle::new(Arc::new(runner))))
        })
    }
}
