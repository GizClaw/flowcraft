//! The three commands.

use flowcraft::error::{Error, Result};
use flowcraft::kernel::config::{Target, parse_target};
use flowcraft::kernel::{Chain, Config, EnvProvider, FileProvider, Platform, SecretProvider};
use flowcraft::logerr;
use flowcraft::resource::PlanError;
use serde::Serialize;
use std::sync::atomic::{AtomicBool, Ordering};
use tracing::Instrument as _;

use crate::admin::{AdminState, serve as serve_admin};
use crate::assembly;
use crate::hosting::HostedCrafts;
use crate::supervisor::{Supervisor, load_deployment};
use crate::watcher::{self, ReloadWatcher};

/// The `check` report, printable as JSON.
#[derive(Debug, Serialize)]
pub struct CheckReport {
    /// Whether planning succeeded.
    pub ok: bool,
    /// How many resources were planned.
    pub resources: usize,
    /// Every planning problem.
    pub errors: Vec<PlanError>,
}

/// Validate a host config (when present) and a deployment document.
///
/// Never starts anything and never opens a socket.
pub fn check(config: Option<&str>, deploys: &[String]) -> Result<CheckReport> {
    let platform = match config {
        Some(path) => {
            let config = Config::parse(&read(path)?)?;
            platform_from(config)?
        }
        None => platform_from(Config::default())?,
    };

    let paths: Vec<std::path::PathBuf> = deploys.iter().map(std::path::PathBuf::from).collect();
    let deployment = match load_deployment(&paths) {
        Ok(deployment) => deployment,
        Err(error) => {
            return Ok(CheckReport {
                ok: false,
                resources: 0,
                errors: vec![PlanError {
                    path: String::new(),
                    message: error.to_string(),
                }],
            });
        }
    };
    let mut document = deployment;
    let mut host_errors = assembly::validate(&platform, &document);
    let externals = match assembly::prepare(&platform, &mut document) {
        Ok(externals) => externals,
        Err(error) => {
            return Ok(CheckReport {
                ok: false,
                resources: document.resources.len(),
                errors: vec![PlanError {
                    path: String::new(),
                    message: error.to_string(),
                }],
            });
        }
    };

    let builder = flowcraft::resource::Builder::new(std::sync::Arc::new(assembly::registry()))
        .with_externals(externals);
    match builder.plan(document.clone()) {
        Ok(plan) => {
            host_errors.shrink_to_fit();
            Ok(CheckReport {
                ok: host_errors.is_empty(),
                resources: plan.order().len(),
                errors: host_errors,
            })
        }
        Err(mut errors) => {
            errors.append(&mut host_errors);
            Ok(CheckReport {
                ok: false,
                resources: document.resources.len(),
                errors,
            })
        }
    }
}

/// Run the platform until a shutdown signal arrives. `SIGHUP` reloads the
/// deployment; `SIGTERM`/`SIGINT` drain and exit.
pub async fn serve(
    config_path: &str,
    deploys: &[String],
    telemetry: std::sync::Arc<crate::logging::Applied>,
) -> Result<()> {
    let config = Config::parse(&read(config_path)?)?;
    // The section can only be applied now: the pipeline had to exist before the
    // file could be read, so anything it cannot change in place is reported as
    // needing a restart rather than silently ignored.
    crate::logging::apply_config(&telemetry, &config.log);
    let platform = platform_from(config)?;

    // One trace for the whole boot. The records below say *what* started; the
    // span says in what order, and how long each step took.
    let startup = tracing::info_span!("startup");
    let (supervisor, kernel, hosted, admin, watcher, ready) = async move {
        tracing::info!(
            flowcraft.endpoints = platform.catalog().len(),
            "platform kernel assembled"
        );
        let layers: Vec<std::path::PathBuf> =
            deploys.iter().map(std::path::PathBuf::from).collect();
        let supervisor = Supervisor::boot(platform.clone(), layers).await?;
        let generation = supervisor.generation().await;
        let entries = supervisor.entries().await;
        tracing::info!(
            flowcraft.gen = generation,
            flowcraft.resources = entries.len(),
            "deployment started"
        );

        let kernel = supervisor.platform();
        let hosted = HostedCrafts::start(kernel.clone(), &kernel.config().crafts).await?;
        if !hosted.is_empty() {
            tracing::info!(flowcraft.crafts = hosted.len(), "crafts hosted");
        }

        let ready = std::sync::Arc::new(AtomicBool::new(true));
        let admin = match admin_socket(&kernel) {
            Some(path) => {
                let state = AdminState::new(
                    supervisor.clone(),
                    hosted.names().into_iter().map(str::to_owned).collect(),
                    ready.clone(),
                    kernel.catalog().len(),
                    telemetry.clone(),
                );
                match serve_admin(&path, state).await {
                    Ok(handle) => {
                        tracing::info!(
                            flowcraft.socket = %path.display(),
                            "admin socket ready"
                        );
                        Some(handle)
                    }
                    // An admin socket is an operator convenience, not a
                    // prerequisite: a read-only /run must not stop the daemon.
                    Err(failure) => {
                        logerr!(warn, failure, "admin socket unavailable");
                        None
                    }
                }
            }
            None => None,
        };

        let watcher: Option<ReloadWatcher> = if watches_bootstrap(&kernel) {
            let dir = std::path::PathBuf::from(&kernel.config().bootstrap.dir);
            match watcher::watch(&dir, supervisor.clone(), ready.clone()) {
                Ok(watcher) => {
                    tracing::info!(
                        flowcraft.dir = %dir.display(),
                        "watching bootstrap secrets for rotation"
                    );
                    Some(watcher)
                }
                // Rotation is an optimization: a failed watch must not stop the daemon.
                Err(failure) => {
                    logerr!(warn, failure, "secret watcher unavailable");
                    None
                }
            }
        } else {
            None
        };

        Ok::<_, Error>((supervisor, kernel, hosted, admin, watcher, ready))
    }
    .instrument(startup)
    .await?;

    wait_for_signals(&supervisor).await;

    // The mirror of `startup`: one trace for the drain, so "what did shutdown
    // touch before it hung" is answerable from the records.
    let shutdown = tracing::info_span!("shutdown");
    async move {
        ready.store(false, Ordering::SeqCst);
        if let Some(watcher) = watcher {
            watcher.stop().await;
        }
        tracing::info!("shutting down");
        if let Some(admin) = admin {
            admin.stop().await;
        }
        hosted.stop().await?;
        let drained = supervisor.shutdown().await;
        kernel.revoke_session(kernel.session())?;
        match drained {
            Ok(()) => {
                tracing::info!("closed");
                Ok(())
            }
            Err(error) => Err(error),
        }
    }
    .instrument(shutdown)
    .await
}

async fn wait_for_signals(supervisor: &Supervisor) {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};

        let mut terminate = match signal(SignalKind::terminate()) {
            Ok(signal) => signal,
            Err(failure) => {
                logerr!(
                    warn,
                    flowcraft::error::Error::from(failure),
                    "cannot install SIGTERM handler"
                );
                return;
            }
        };
        let mut hangup = match signal(SignalKind::hangup()) {
            Ok(signal) => signal,
            Err(failure) => {
                logerr!(
                    warn,
                    flowcraft::error::Error::from(failure),
                    "cannot install SIGHUP handler"
                );
                return;
            }
        };
        let ctrl_c = tokio::signal::ctrl_c();
        tokio::pin!(ctrl_c);
        loop {
            tokio::select! {
                _ = &mut ctrl_c => return,
                _ = terminate.recv() => return,
                _ = hangup.recv() => {
                    match supervisor.reload().await {
                        Ok(outcome) => tracing::info!(
                            flowcraft.gen.previous = outcome.previous,
                            flowcraft.gen = outcome.current,
                            flowcraft.resources = outcome.resources,
                            flowcraft.gen.drained = outcome.drained,
                            "reloaded"
                        ),
                        // A bad document must never cost a healthy deployment.
                        Err(failure) => logerr!(
                            error,
                            failure,
                            "reload failed; keeping the running generation"
                        ),
                    }
                }
            }
        }
    }
    #[cfg(not(unix))]
    {
        let _ = supervisor;
        let _ = tokio::signal::ctrl_c().await;
    }
}

/// Read just the `log:` section, for the pipeline that must exist before the
/// authoritative parse.
///
/// A failure here is silent on purpose: `serve` parses the same document a
/// moment later and reports the real error, through a pipeline that is already
/// running. This is a peek, not a validation.
#[must_use]
pub fn peek_log(path: &str) -> Option<flowcraft::kernel::config::LogConfig> {
    let bytes = std::fs::read(path).ok()?;
    let config = Config::parse(&bytes).ok()?;
    (!config.log.is_empty()).then_some(config.log)
}

/// Build the kernel with the bootstrap secret sources the host configured.
fn platform_from(config: Config) -> Result<Platform> {
    let mut providers: Vec<std::sync::Arc<dyn SecretProvider>> = Vec::new();
    if !config.bootstrap.env_prefix.is_empty() {
        providers.push(std::sync::Arc::new(EnvProvider::new(
            config.bootstrap.env_prefix.clone(),
        )));
    }
    if !config.bootstrap.dir.is_empty() {
        providers.push(std::sync::Arc::new(FileProvider::new(
            config.bootstrap.dir.clone(),
        )?));
    }
    Platform::with_secrets(config, std::sync::Arc::new(Chain::new(providers)))
}

fn read(path: &str) -> Result<Vec<u8>> {
    std::fs::read(path).map_err(|error| Error::not_found(format!("cannot read {path}: {error}")))
}

/// A minimal craft endpoint, used by tests and by `examples/` to exercise the
/// ABI without a Go build.
pub async fn craft_stub(
    socket: &str,
    name: &str,
    renew_log: Option<&str>,
    token: Option<&str>,
    abi_version: Option<&str>,
    provides: Option<&str>,
    bad_envelope: bool,
) -> Result<()> {
    use flowcraft::proto::craft as pb;
    use flowcraft::proto::craft::craft_server;
    use tokio_stream::wrappers::UnixListenerStream;
    use tonic::{Request, Response, Status};

    struct StubService {
        name: String,
        renew_log: Option<std::path::PathBuf>,
        token: Option<String>,
        abi_version: String,
        provides: Vec<String>,
        bad_envelope: bool,
        closed: std::sync::Mutex<std::collections::BTreeSet<String>>,
    }

    impl StubService {
        fn require_token<T>(&self, request: &Request<T>) -> std::result::Result<(), Status> {
            let Some(expected) = &self.token else {
                return Ok(());
            };
            let presented = request
                .metadata()
                .get("x-flowcraft-craft-token")
                .and_then(|value| value.to_str().ok());
            if presented == Some(expected.as_str()) {
                Ok(())
            } else {
                Err(Status::unauthenticated("craft: missing or wrong token"))
            }
        }

        fn ensure_open(&self, session: &str) -> std::result::Result<(), Status> {
            if self
                .closed
                .lock()
                .map(|closed| closed.contains(session))
                .unwrap_or(false)
            {
                return Err(Status::not_found(format!(
                    "craft: session {session:?} is closed"
                )));
            }
            Ok(())
        }
    }

    type RunStream = std::pin::Pin<
        Box<
            dyn tokio_stream::Stream<Item = std::result::Result<pb::RunEvent, Status>>
                + Send
                + 'static,
        >,
    >;

    #[tonic::async_trait]
    impl craft_server::Craft for StubService {
        type RunStream = RunStream;

        async fn describe(
            &self,
            _request: Request<pb::DescribeRequest>,
        ) -> std::result::Result<Response<pb::DescribeResponse>, Status> {
            Ok(Response::new(pb::DescribeResponse {
                info: Some(pb::CraftInfo {
                    name: self.name.clone(),
                    version: env!("CARGO_PKG_VERSION").to_owned(),
                    requires: Vec::new(),
                    provides: self.provides.clone(),
                    tenancy: "single".to_owned(),
                    abi_version: self.abi_version.clone(),
                    abi_versions: Vec::new(),
                    input_types: vec!["stub.text".to_owned()],
                    event_types: vec!["stub.text".to_owned()],
                }),
            }))
        }

        async fn start(
            &self,
            request: Request<pb::StartRequest>,
        ) -> std::result::Result<Response<pb::StartResponse>, Status> {
            self.require_token(&request)?;
            Ok(Response::new(pb::StartResponse {
                session: "stub-session".to_owned(),
                grants: request.get_ref().grants.clone(),
            }))
        }

        async fn run(
            &self,
            request: Request<pb::RunRequest>,
        ) -> std::result::Result<Response<Self::RunStream>, Status> {
            self.require_token(&request)?;
            self.ensure_open(&request.get_ref().session)?;
            let payload = if self.bad_envelope {
                // A delta event with no body: the platform must reject it.
                pb::Envelope { body: None }
            } else {
                crate::abi::delta_text("hello from the stub")
            };
            let events: Vec<std::result::Result<pb::RunEvent, Status>> = vec![
                Ok(pb::RunEvent {
                    seq: 1,
                    r#type: "delta".to_owned(),
                    payload: Some(payload),
                    error: String::new(),
                    credit: 0,
                }),
                Ok(pb::RunEvent {
                    seq: 2,
                    r#type: "done".to_owned(),
                    payload: None,
                    error: String::new(),
                    credit: 0,
                }),
            ];
            Ok(Response::new(Box::pin(tokio_stream::iter(events))))
        }

        async fn steer(
            &self,
            request: Request<pb::SteerRequest>,
        ) -> std::result::Result<Response<pb::SteerResponse>, Status> {
            self.require_token(&request)?;
            Ok(Response::new(pb::SteerResponse {}))
        }

        async fn renew(
            &self,
            request: Request<pb::RenewRequest>,
        ) -> std::result::Result<Response<pb::RenewResponse>, Status> {
            self.require_token(&request)?;
            if let Some(path) = &self.renew_log {
                use std::io::Write;
                if let Ok(mut file) = std::fs::OpenOptions::new()
                    .create(true)
                    .append(true)
                    .open(path)
                {
                    let _ = writeln!(file, "renew {}", request.get_ref().session);
                }
            }
            Ok(Response::new(pb::RenewResponse {
                grants: request.get_ref().grants.clone(),
            }))
        }

        async fn cancel(
            &self,
            request: Request<pb::CancelRequest>,
        ) -> std::result::Result<Response<pb::CancelResponse>, Status> {
            self.require_token(&request)?;
            Ok(Response::new(pb::CancelResponse {}))
        }

        async fn close(
            &self,
            request: Request<pb::CloseRequest>,
        ) -> std::result::Result<Response<pb::CloseResponse>, Status> {
            self.require_token(&request)?;
            if let Ok(mut closed) = self.closed.lock() {
                closed.insert(request.get_ref().session.clone());
            }
            Ok(Response::new(pb::CloseResponse {}))
        }
    }

    let path = std::path::PathBuf::from(socket);
    if path.exists() {
        let _ = std::fs::remove_file(&path);
    }
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let listener = tokio::net::UnixListener::bind(&path)?;
    tracing::info!(
        flowcraft.socket = %path.display(),
        flowcraft.craft = name,
        "craft stub listening"
    );
    tonic::transport::Server::builder()
        .add_service(craft_server::CraftServer::new(StubService {
            name: name.to_owned(),
            renew_log: renew_log.map(std::path::PathBuf::from),
            token: token
                .map(str::to_owned)
                .or_else(|| std::env::var("FLOWCRAFT_CRAFT_TOKEN").ok()),
            abi_version: abi_version.unwrap_or(crate::abi::ABI_VERSION).to_owned(),
            provides: provides.map(|cap| vec![cap.to_owned()]).unwrap_or_default(),
            bad_envelope,
            closed: std::sync::Mutex::new(std::collections::BTreeSet::new()),
        }))
        .serve_with_incoming(UnixListenerStream::new(listener))
        .await
        .map_err(|error| Error::internal(format!("craft stub: {error}")))
}

fn admin_socket(platform: &Platform) -> Option<std::path::PathBuf> {
    match parse_target(&platform.config().listen.admin) {
        Ok(Target::Unix(path)) => Some(path),
        Ok(Target::Dns(_)) => {
            tracing::warn!("admin socket must be a unix target");
            None
        }
        Err(failure) => {
            logerr!(warn, failure, "admin socket target is invalid");
            None
        }
    }
}

/// One request line to the admin socket, one response line back.
async fn admin_call(socket: &str, request: serde_json::Value) -> Result<serde_json::Value> {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    let stream = tokio::net::UnixStream::connect(socket)
        .await
        .map_err(|error| Error::not_available(format!("admin socket {socket}: {error}")))?;
    let (read, mut write) = stream.into_split();
    write
        .write_all(format!("{request}\n").as_bytes())
        .await
        .map_err(Error::from)?;
    write.shutdown().await.map_err(Error::from)?;

    let mut reader = BufReader::new(read);
    let mut line = String::new();
    reader.read_line(&mut line).await.map_err(Error::from)?;
    serde_json::from_str(line.trim())
        .map_err(|error| Error::internal(format!("admin response: {error}")))
}

/// One command against a running daemon's admin socket.
pub async fn ctl(socket: &str, command: &str, session: Option<&str>) -> Result<()> {
    let mut request = serde_json::json!({"command": command});
    if let Some(session) = session {
        request["session"] = serde_json::json!(session);
    }
    let response = admin_call(socket, request).await?;
    println!(
        "{}",
        serde_json::to_string_pretty(&response).unwrap_or_default()
    );
    if response.get("ok").and_then(|value| value.as_bool()) == Some(false) {
        let message = response
            .get("error")
            .and_then(|value| value.as_str())
            .unwrap_or("admin command failed");
        return Err(Error::internal(message.to_owned()));
    }
    Ok(())
}

/// Write a support bundle for a running daemon.
///
/// The bundle is a directory, not an archive: no compression dependency, and the
/// user can look inside before sending it. What it must *not* contain is as
/// important as what it must: the identity file's salt and any grant token stay
/// in the daemon.
pub async fn diag(socket: &str, out: &str) -> Result<()> {
    let response = admin_call(socket, serde_json::json!({"command": "diag"})).await?;
    if response.get("ok").and_then(|value| value.as_bool()) != Some(true) {
        let message = response
            .get("error")
            .and_then(|value| value.as_str())
            .unwrap_or("the daemon refused to produce a bundle");
        return Err(Error::internal(message.to_owned()));
    }

    let directory = std::path::Path::new(out);
    std::fs::create_dir_all(directory)
        .map_err(|error| Error::internal(format!("bundle {}: {error}", directory.display())))?;

    let ring = response
        .get("ring")
        .and_then(|value| value.as_str())
        .unwrap_or_default();
    write_bundle(directory, "telemetry.log", ring)?;
    write_bundle(
        directory,
        "manifest.json",
        &serde_json::to_string_pretty(&without(&response, "ring")).unwrap_or_default(),
    )?;
    let catalog = flowcraft::resource::Catalog::from_registry(&assembly::registry());
    write_bundle(
        directory,
        "catalog.json",
        &serde_json::to_string_pretty(&catalog).unwrap_or_default(),
    )?;

    let install = response
        .get("install")
        .and_then(|value| value.as_str())
        .unwrap_or_default();
    let support_code: String = install.chars().take(8).collect();
    write_bundle(
        directory,
        "README.txt",
        &format!(
            "FlowCraft support bundle\n\
             \n\
             support code: {support_code}\n\
             install: {install}\n\
             version: {}\n\
             session: {}\n\
             \n\
             telemetry.log holds the most recent records this daemon kept in\n\
             memory; manifest.json describes the pipeline that produced them;\n\
             catalog.json lists the capabilities this build provides.\n\
             Nothing here contains a grant token, a secret value or the identity\n\
             salt.\n",
            response
                .get("version")
                .and_then(|value| value.as_str())
                .unwrap_or_default(),
            response
                .get("session")
                .and_then(|value| value.as_str())
                .unwrap_or_default(),
        ),
    )?;

    println!("support code: {support_code}");
    println!("bundle: {}", directory.display());
    Ok(())
}

fn write_bundle(directory: &std::path::Path, name: &str, contents: &str) -> Result<()> {
    let path = directory.join(name);
    std::fs::write(&path, contents)
        .map_err(|error| Error::internal(format!("bundle {}: {error}", path.display())))
}

/// The response without one key, for the manifest.
fn without(response: &serde_json::Value, key: &str) -> serde_json::Value {
    let mut copy = response.clone();
    if let Some(object) = copy.as_object_mut() {
        object.remove(key);
    }
    copy
}

fn watches_bootstrap(platform: &Platform) -> bool {
    let bootstrap = &platform.config().bootstrap;
    !bootstrap.dir.is_empty() && bootstrap.watch.unwrap_or(true)
}
