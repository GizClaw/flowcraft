//! The remote client: a remote runner presented as the local contract.
//!
//! Capabilities are fetched once, lazily, and then reported verbatim, so a
//! remote runner declares exactly what its server declared — never less (which
//! would look like a downgrade) and never more (which would be a lie).
//!
//! A remote client can only address the sessions *it* started: the wire's
//! `List` is the way to see what else exists, and `Session::features` from the
//! start response is authoritative (the wire's `SessionInfo` carries only
//! `tty`).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

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
use tokio::sync::{Mutex as AsyncMutex, mpsc};
use tokio_stream::wrappers::ReceiverStream;

use crate::codes::{self, Code};
use crate::contract::{
    CONTRACT, Capabilities, EventStream, ExitStatus, Handle, KIND, ReadResult, Runner, Session,
    SessionFeatures, SessionInfo, SessionSpec, Signal,
};
use crate::journal::{Batch, Journal};
use crate::wire;

/// The implementation name.
pub const IMPL: &str = "remote";

/// What this client remembers about one started session.
#[derive(Clone)]
struct SessionRef {
    pid: Option<u32>,
    features: SessionFeatures,
    /// Shared with the handle, so `wait` on one view is visible from the other.
    exit: Arc<Mutex<Option<ExitStatus>>>,
}

/// A runner served by another process.
pub struct RemoteRunner {
    wire: Arc<Wire>,
    capabilities: std::sync::Mutex<Option<Capabilities>>,
    sessions: AsyncMutex<BTreeMap<String, SessionRef>>,
}

/// One session of a remote runner.
pub struct RemoteSession {
    wire: Arc<Wire>,
    id: String,
    pid: Option<u32>,
    features: SessionFeatures,
    exit: Arc<Mutex<Option<ExitStatus>>>,
    /// Closed locally, so every later call answers `Closed` without a round
    /// trip — and the same answer the in-process backends give, instead of the
    /// server's `NotFound` for an id it already forgot.
    closed: Arc<std::sync::atomic::AtomicBool>,
}

impl std::fmt::Debug for RemoteRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.RemoteRunner")
            .field("wire", &self.wire)
            .finish_non_exhaustive()
    }
}

impl RemoteSession {
    fn ensure_open(&self) -> Result<()> {
        if self.closed.load(std::sync::atomic::Ordering::Acquire) {
            return Err(Error::closed("sandbox: session is closed"));
        }
        Ok(())
    }

    fn mark_closed(&self) {
        self.closed
            .store(true, std::sync::atomic::Ordering::Release);
    }
}

impl std::fmt::Debug for RemoteSession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.RemoteSession")
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
            sessions: AsyncMutex::new(BTreeMap::new()),
        })
    }
}

#[async_trait]
impl Runner for RemoteRunner {
    fn capabilities(&self) -> BoxFuture<'_, Result<Capabilities>> {
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
            let capabilities = wire::capabilities_from_proto(described);
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
                        spec: Some(wire::spec_to_proto(&spec)),
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
            .map(wire::features_from_proto)
            .unwrap_or_default();
        let exit = Arc::new(Mutex::new(None));
        let session: Arc<dyn Session> = Arc::new(RemoteSession {
            wire: self.wire.clone(),
            id: reference.id.clone(),
            pid,
            features,
            exit: exit.clone(),
            closed: Arc::new(std::sync::atomic::AtomicBool::new(false)),
        });
        self.sessions.lock().await.insert(
            reference.id,
            SessionRef {
                pid,
                features,
                exit,
            },
        );
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
            exit: reference.exit.clone(),
            closed: Arc::new(std::sync::atomic::AtomicBool::new(false)),
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
            .map(wire::info_from_proto)
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

    async fn close_session(&self, id: &str) -> Result<()> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .close_session(
                self.wire
                    .request(pb::CloseSessionRequest {
                        session: id.to_owned(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        self.sessions.lock().await.remove(id);
        Ok(())
    }

    async fn open_journal(&self) -> Result<Arc<dyn Journal>> {
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .open_journal(self.wire.request(pb::OpenJournalRequest {}).await?)
            .await
            .map_err(from_status)?
            .into_inner();
        if response.journal.is_empty() {
            return Err(Error::internal("sandbox: server returned no journal"));
        }
        Ok(Arc::new(RemoteJournal {
            wire: self.wire.clone(),
            id: response.journal,
            closed: std::sync::atomic::AtomicBool::new(false),
        }))
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

    fn exit(&self) -> Option<ExitStatus> {
        self.exit.lock().ok().and_then(|slot| slot.clone())
    }

    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult> {
        self.ensure_open()?;
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
        Ok(wire::read_from_proto(response))
    }

    async fn write(&self, data: &[u8]) -> Result<()> {
        self.ensure_open()?;
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
        self.ensure_open()?;
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

    async fn resize(&self, rows: u16, cols: u16) -> Result<()> {
        self.ensure_open()?;
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .resize(
                self.wire
                    .request(pb::ResizeRequest {
                        session: self.id.clone(),
                        rows: i32::from(rows),
                        cols: i32::from(cols),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }

    async fn signal(&self, signal: Signal) -> Result<()> {
        self.ensure_open()?;
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .signal(
                self.wire
                    .request(pb::SignalRequest {
                        session: self.id.clone(),
                        signal: wire::signal_to_proto(signal),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        Ok(())
    }

    async fn terminate(&self) -> Result<()> {
        self.ensure_open()?;
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

    async fn wait(&self) -> Result<ExitStatus> {
        self.ensure_open()?;
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
        let exit = response.exit.map(wire::exit_from_proto).unwrap_or_default();
        if let Ok(mut slot) = self.exit.lock() {
            *slot = Some(exit.clone());
        }
        // The server reports a timeout or an exhausted budget as a failed call;
        // the classification travels with the outcome either way.
        if let Some(error) = exit.error() {
            return Err(error);
        }
        Ok(exit)
    }

    fn watch(&self) -> Result<EventStream> {
        self.ensure_open()?;
        let wire = self.wire.clone();
        let id = self.id.clone();
        let (sender, receiver) = mpsc::channel(64);
        tokio::spawn(async move {
            let Ok(request) = wire.request(pb::WatchRequest { session: id }).await else {
                return;
            };
            let Ok(channel) = wire.channel().await else {
                return;
            };
            let Ok(response) = SandboxClient::new(channel).watch(request).await else {
                return;
            };
            let mut stream = response.into_inner();
            loop {
                match stream.message().await {
                    Ok(Some(event)) => match wire::event_from_proto(event) {
                        Ok(event) => {
                            if sender.send(event).await.is_err() {
                                return;
                            }
                        }
                        Err(error) => {
                            tracing::debug!(%error, "sandbox: dropping an unreadable event");
                        }
                    },
                    Ok(None) => return,
                    Err(error) => {
                        tracing::debug!(%error, "sandbox: watch stream ended");
                        return;
                    }
                }
            }
        });
        Ok(Box::pin(ReceiverStream::new(receiver)))
    }

    async fn close(&self) -> Result<()> {
        if self.closed.load(std::sync::atomic::Ordering::Acquire) {
            return Ok(());
        }
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .close_session(
                self.wire
                    .request(pb::CloseSessionRequest {
                        session: self.id.clone(),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?;
        self.mark_closed();
        Ok(())
    }
}

/// A journal served by another process.
struct RemoteJournal {
    wire: Arc<Wire>,
    id: String,
    closed: std::sync::atomic::AtomicBool,
}

impl std::fmt::Debug for RemoteJournal {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.RemoteJournal")
            .field("id", &self.id)
            .finish_non_exhaustive()
    }
}

#[async_trait]
impl Journal for RemoteJournal {
    async fn read(&self, after_seq: u64, max: usize) -> Result<Batch> {
        if self.closed.load(std::sync::atomic::Ordering::Acquire) {
            return Err(Error::closed("sandbox: journal is closed"));
        }
        let mut client = SandboxClient::new(self.wire.channel().await?);
        let response = client
            .journal_read(
                self.wire
                    .request(pb::JournalReadRequest {
                        journal: self.id.clone(),
                        after_seq: i64::try_from(after_seq).unwrap_or(i64::MAX),
                        max: i32::try_from(max).unwrap_or(i32::MAX),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        wire::batch_from_proto(response)
    }

    async fn close(&self) -> Result<()> {
        if self.closed.swap(true, std::sync::atomic::Ordering::AcqRel) {
            return Ok(());
        }
        let mut client = SandboxClient::new(self.wire.channel().await?);
        client
            .close_journal(
                self.wire
                    .request(pb::CloseJournalRequest {
                        journal: self.id.clone(),
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
