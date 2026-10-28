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
        let admin = match admin_target(&kernel) {
            Some(target) => {
                let state = AdminState::new(
                    supervisor.clone(),
                    hosted.names().into_iter().map(str::to_owned).collect(),
                    ready.clone(),
                    kernel.catalog().len(),
                    telemetry.clone(),
                );
                let (tls, token) = match admin_credentials(&kernel, &target) {
                    Ok(credentials) => credentials,
                    Err(failure) => {
                        logerr!(warn, failure, "admin listener unavailable");
                        return Err(failure);
                    }
                };
                match serve_admin(&target, tls, token, state).await {
                    Ok(handle) => {
                        tracing::info!(
                            flowcraft.admin = %target.as_uri(),
                            "admin listener ready"
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
// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
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

// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
fn read(path: &str) -> Result<Vec<u8>> {
    std::fs::read(path).map_err(|error| Error::not_found(format!("cannot read {path}: {error}")))
}

/// How `craft-stub` listens.
///
/// The ABI is transport-agnostic on purpose: a craft is a `unix://` socket when
/// the platform and the craft share a host, and a `dns://` target with TLS when
/// they do not. The double supports both so the second path is not code that
/// only production exercises.
pub struct StubServe {
    /// `unix:///abs/path` or `dns:///host:port`.
    pub target: String,
    /// Server certificate chain (PEM path), required for a `dns` target.
    pub cert: Option<String>,
    /// Server private key (PEM path), required for a `dns` target.
    pub key: Option<String>,
    /// Client CA bundle (PEM path), which turns the listener into mutual TLS.
    pub client_ca: Option<String>,
}

/// A minimal craft endpoint, used by tests and by `examples/` to exercise the
/// ABI without a Go build.
///
/// The behaviour lives in [`crate::stub::StubCraft`]; this owns the listener and
/// hands the handler to the generic adapter, which is also what a host serving a
/// real craft does.
// One-shot CLI (`flowcraftd craft --stub`): the process has this one job and exits.
#[allow(clippy::disallowed_methods)]
pub async fn craft_stub(
    serve: &StubServe,
    name: &str,
    renew_log: Option<&str>,
    token: Option<&str>,
    abi_version: Option<&str>,
    provides: Option<&str>,
    bad_envelope: bool,
) -> Result<()> {
    use flowcraft::craft::Adapter;
    use flowcraft::kernel::Target;
    use flowcraft::kernel::config::parse_target;
    use flowcraft::proto::craft::craft_server;

    let target = parse_target(&serve.target)?;
    let handler = std::sync::Arc::new(crate::stub::StubCraft::new(
        name,
        abi_version,
        provides,
        renew_log,
        bad_envelope,
    ));
    let mut adapter = Adapter::new(handler);
    if let Some(token) = token
        .map(str::to_owned)
        .or_else(|| std::env::var("FLOWCRAFT_CRAFT_TOKEN").ok())
    {
        adapter = adapter.with_token(token);
    }
    tracing::info!(
        flowcraft.target = %target.as_uri(),
        flowcraft.craft = name,
        "craft stub listening"
    );

    #[allow(unused_mut)]
    let mut builder = tonic::transport::Server::builder();
    match target {
        #[cfg(unix)]
        Target::Unix(path) => {
            if path.exists() {
                let _ = std::fs::remove_file(&path);
            }
            if let Some(parent) = path.parent() {
                std::fs::create_dir_all(parent)?;
            }
            let listener = tokio::net::UnixListener::bind(&path)?;
            builder
                .add_service(craft_server::CraftServer::new(adapter))
                .serve_with_incoming(tokio_stream::wrappers::UnixListenerStream::new(listener))
                .await
                .map_err(|error| Error::internal(format!("craft stub: {error}")))
        }
        #[cfg(not(unix))]
        Target::Unix(path) => Err(Error::not_available(format!(
            "craft-stub: {} is a unix socket and this host has none; use a dns target",
            path.display()
        ))),
        Target::Dns(address) => {
            let tls = stub_tls(serve)?;
            let listener = std::net::TcpListener::bind(&address).map_err(|error| {
                Error::not_available(format!("craft-stub: cannot bind {address}: {error}"))
            })?;
            listener.set_nonblocking(true)?;
            let listener = tokio::net::TcpListener::from_std(listener)?;
            builder
                .tls_config(tls)
                .map_err(|error| Error::validation(format!("craft-stub: tls: {error}")))?
                .add_service(craft_server::CraftServer::new(adapter))
                .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
                .await
                .map_err(|error| Error::internal(format!("craft stub: {error}")))
        }
    }
}

/// The server half of a `dns` stub: certificate, key, and an optional client CA
/// that makes the listener mutual TLS.
// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
fn stub_tls(serve: &StubServe) -> Result<tonic::transport::ServerTlsConfig> {
    use tonic::transport::{Certificate, Identity, ServerTlsConfig};

    let (Some(cert), Some(key)) = (serve.cert.as_deref(), serve.key.as_deref()) else {
        return Err(Error::validation(
            "craft-stub: a dns target needs --tls-cert and --tls-key",
        ));
    };
    let read = |path: &str| {
        std::fs::read(path)
            .map_err(|error| Error::not_found(format!("craft-stub: cannot read {path}: {error}")))
    };
    let mut tls = ServerTlsConfig::new().identity(Identity::from_pem(read(cert)?, read(key)?));
    if let Some(ca) = &serve.client_ca {
        tls = tls.client_ca_root(Certificate::from_pem(read(ca)?));
    }
    Ok(tls)
}

/// Where the admin surface listens, or `None` when the target is unusable.
fn admin_target(platform: &Platform) -> Option<Target> {
    match parse_target(&platform.config().listen.admin) {
        Ok(target) => Some(target),
        Err(failure) => {
            logerr!(warn, failure, "admin target is invalid");
            None
        }
    }
}

/// TLS material and bearer token for a `dns` admin target.
///
/// A unix socket needs neither: its permissions and the kernel's peer
/// credentials are the gate. A `dns` target needs both, because TLS proves only
/// the server's identity and the socket's file mode no longer applies.
fn admin_credentials(
    platform: &Platform,
    target: &Target,
) -> Result<(Option<tokio_rustls::TlsAcceptor>, Option<String>)> {
    let listen = &platform.config().listen;
    let Target::Dns(_) = target else {
        return Ok((None, None));
    };
    let tls = listen
        .admin_tls
        .as_ref()
        .ok_or_else(|| Error::validation("listen.admin_tls is required on a dns admin target"))?;
    let secrets = platform.secrets();
    let read = |field: &str, reference: &str| -> Result<Vec<u8>> {
        flowcraft::kernel::secret_provider::resolve(
            &format!("listen.admin_tls.{field}"),
            reference,
            &*secrets,
        )?
        .ok_or_else(|| {
            Error::validation(format!(
                "listen.admin_tls.{field} is required on a dns admin target"
            ))
        })
    };
    let acceptor = crate::admin::acceptor(&read("cert", &tls.cert)?, &read("key", &tls.key)?)?;

    // A fresh token per start: a long-lived one only adds a window in which a
    // stale copy is worth stealing.
    let path = listen.admin_token_file.trim();
    if path.is_empty() {
        return Err(Error::validation(
            "listen.admin_token_file is required on a dns admin target",
        ));
    }
    let token = crate::admin::mint_token();
    crate::admin::write_token(std::path::Path::new(path), &token)
        .map_err(|error| Error::internal(format!("admin token file {path}: {error}")))?;
    Ok((Some(acceptor), Some(token)))
}

/// How `ctl` and `diag` reach a running daemon.
///
/// A unix socket needs nothing but its path; a `dns` target needs the CA to
/// verify the daemon and the token file the daemon wrote.
pub struct AdminClient {
    /// `unix:///abs/path` or `dns:///host:port`.
    pub target: String,
    /// CA bundle that signed the daemon's certificate (a `dns` target).
    pub ca: Option<String>,
    /// Name to verify in that certificate; defaults to the address's host.
    pub server_name: Option<String>,
    /// File the daemon wrote its bearer token to (a `dns` target).
    pub token_file: Option<String>,
    /// The token, inline, instead of a file.
    pub token: Option<String>,
}

/// One admin command over either transport: an HTTP request, one JSON answer.
///
/// A bare path counts as `unix://`, because `ctl --socket /run/…/admin.sock` is
/// the shape every script and fixture already writes.
pub async fn call(
    client: &AdminClient,
    command: &str,
    session: Option<&str>,
) -> Result<serde_json::Value> {
    use http_body_util::BodyExt as _;

    let spelled = if client.target.contains("://") {
        client.target.clone()
    } else {
        format!("unix://{}", client.target)
    };
    let target = parse_target(&spelled)?;
    let (method, path) = match command {
        "status" => ("GET", "/status"),
        "reload" => ("POST", "/reload"),
        "drain" => ("POST", "/drain"),
        "crafts" => ("GET", "/crafts"),
        "diag" => ("GET", "/diag"),
        "revoke" => ("POST", "/revoke"),
        other => {
            return Err(Error::validation(format!(
                "unknown admin command {other:?}"
            )));
        }
    };
    let body = if command == "revoke" {
        serde_json::to_vec(&serde_json::json!({ "session": session.unwrap_or_default() }))
            .map_err(|error| Error::internal(format!("admin request: {error}")))?
    } else {
        Vec::new()
    };

    let mut builder = hyper::Request::builder()
        .method(method)
        .uri(path)
        .header(hyper::header::HOST, "flowcraft.local");
    if !body.is_empty() {
        builder = builder.header(hyper::header::CONTENT_TYPE, "application/json");
    }
    if let Some(token) = admin_token(client, &target)? {
        builder = builder.header(hyper::header::AUTHORIZATION, format!("Bearer {token}"));
    }
    let request = builder
        .body(http_body_util::Full::new(hyper::body::Bytes::from(body)))
        .map_err(|error| Error::internal(format!("admin request: {error}")))?;

    let response = match target {
        #[cfg(unix)]
        Target::Unix(path) => {
            let stream = tokio::net::UnixStream::connect(&path)
                .await
                .map_err(|error| {
                    Error::not_available(format!("admin socket {}: {error}", path.display()))
                })?;
            let (mut sender, connection) =
                hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(stream))
                    .await
                    .map_err(|error| Error::internal(format!("admin handshake: {error}")))?;
            tokio::spawn(connection);
            sender
                .send_request(request)
                .await
                .map_err(|error| Error::not_available(format!("admin request: {error}")))?
        }
        #[cfg(not(unix))]
        Target::Unix(path) => {
            return Err(Error::not_available(format!(
                "admin socket {}: this host has no unix sockets; use a dns target",
                path.display()
            )));
        }
        Target::Dns(address) => {
            let stream = tokio::net::TcpStream::connect(&address)
                .await
                .map_err(|error| Error::not_available(format!("admin {address}: {error}")))?;
            let (connector, name) = admin_client_tls(client, &address)?;
            let stream = connector
                .connect(name, stream)
                .await
                .map_err(|error| Error::not_available(format!("admin tls: {error}")))?;
            let (mut sender, connection) =
                hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(stream))
                    .await
                    .map_err(|error| Error::internal(format!("admin handshake: {error}")))?;
            tokio::spawn(connection);
            sender
                .send_request(request)
                .await
                .map_err(|error| Error::not_available(format!("admin request: {error}")))?
        }
    };

    let bytes = response
        .into_body()
        .collect()
        .await
        .map_err(|error| Error::internal(format!("admin response: {error}")))?
        .to_bytes();
    serde_json::from_slice(&bytes)
        .map_err(|error| Error::internal(format!("admin response: {error}")))
}

/// The token to present, if this transport needs one.
// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
fn admin_token(client: &AdminClient, target: &Target) -> Result<Option<String>> {
    if let Some(token) = &client.token {
        return Ok(Some(token.trim().to_owned()));
    }
    if let Some(path) = &client.token_file {
        let text = std::fs::read_to_string(path)
            .map_err(|error| Error::not_available(format!("admin token file {path}: {error}")))?;
        return Ok(Some(text.trim().to_owned()));
    }
    match target {
        // Nothing secret: the socket's mode and the peer credentials are the
        // gate, and the daemon checks them.
        #[cfg(unix)]
        Target::Unix(_) => Ok(None),
        #[cfg(not(unix))]
        Target::Unix(_) => Ok(None),
        Target::Dns(_) => Err(Error::validation(
            "a dns admin target needs --token-file (or --token)",
        )),
    }
}

/// The TLS client a `dns` admin target needs: verify the daemon against `--ca`.
// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
fn admin_client_tls(
    client: &AdminClient,
    address: &str,
) -> Result<(
    tokio_rustls::TlsConnector,
    rustls_pki_types::ServerName<'static>,
)> {
    use rustls_pki_types::pem::PemObject as _;
    use rustls_pki_types::{CertificateDer, ServerName};

    let path = client
        .ca
        .as_deref()
        .ok_or_else(|| Error::validation("a dns admin target needs --ca"))?;
    let pem = std::fs::read(path)
        .map_err(|error| Error::not_available(format!("admin ca {path}: {error}")))?;
    let mut roots = rustls::RootCertStore::empty();
    for certificate in CertificateDer::pem_slice_iter(&pem) {
        let certificate =
            certificate.map_err(|error| Error::validation(format!("admin ca: {error}")))?;
        roots
            .add(certificate)
            .map_err(|error| Error::validation(format!("admin ca: {error}")))?;
    }

    let host = client
        .server_name
        .clone()
        .unwrap_or_else(|| default_server_name(address));
    let name = ServerName::try_from(host)
        .map_err(|error| Error::validation(format!("admin server name: {error}")))?;

    crate::kernel_tls_provider();
    let config = rustls::ClientConfig::builder()
        .with_root_certificates(roots)
        .with_no_client_auth();
    Ok((
        tokio_rustls::TlsConnector::from(std::sync::Arc::new(config)),
        name,
    ))
}

/// `127.0.0.1:17777` → `127.0.0.1`; `[::1]:17777` → `::1`.
fn default_server_name(address: &str) -> String {
    match address.rsplit_once(':') {
        Some((host, _)) => host
            .trim_start_matches('[')
            .trim_end_matches(']')
            .to_owned(),
        None => address.to_owned(),
    }
}

/// One command against a running daemon's admin socket.
pub async fn ctl(client: &AdminClient, command: &str, session: Option<&str>) -> Result<()> {
    let response = call(client, command, session).await?;
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
// One-shot CLI (`flowcraftd diag`): the process has this one job and exits.
#[allow(clippy::disallowed_methods)]
pub async fn diag(client: &AdminClient, out: &str) -> Result<()> {
    let response = call(client, "diag", None).await?;
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

// One-shot CLI (`flowcraftd <verb>`): the process has this one job and exits,
// so there is no other task on the executor for a blocking call to starve.
#[allow(clippy::disallowed_methods)]
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
