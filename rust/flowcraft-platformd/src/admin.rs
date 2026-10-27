//! The admin surface: HTTP/1.1 over a local listener the host names.
//!
//! One protocol, two transports, and the transport decides how a caller is
//! identified:
//!
//! * `unix://…` — directory `0700`, socket `0600`, and the kernel's peer
//!   credentials on top: only this uid or root may drive it. Nothing secret is
//!   involved, which is why this stays the default where it exists.
//! * `dns:///host:port` — TCP with TLS, mandatory, plus a bearer token from a
//!   file only this user can read. TLS proves the server's identity, not the
//!   caller's; the token is what carries "the same user" to a host without unix
//!   sockets.
//!
//! The verbs stay few — `status`, `reload`, `drain`, `crafts`, `revoke`, `diag`
//! — because everything else is deployment-shaped and belongs in the documents.
//! `ok` in the body is the business answer; the HTTP status only speaks about
//! the request itself.
//!
//! Every command is logged: this surface can stop a deployment, and "who asked
//! for that" has to be answerable from the records.

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;

use flowcraft::error::{Error, Result};
use flowcraft::kernel::Target;
use flowcraft::logerr;
use http_body_util::{BodyExt as _, Full, LengthLimitError, Limited};
use hyper::body::Bytes;
use hyper::service::service_fn;
use hyper::{Request, Response, StatusCode, header};
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use tokio::sync::Mutex;
use tracing::Instrument as _;

use crate::logging::Applied;
use crate::supervisor::Supervisor;

/// What the admin surface reports and can change.
pub struct AdminState {
    supervisor: Arc<Supervisor>,
    crafts: Vec<String>,
    ready: Arc<AtomicBool>,
    endpoints: usize,
    started_at: Instant,
    reload_gate: Mutex<()>,
    telemetry: Arc<Applied>,
}

impl AdminState {
    /// Build the state for one running daemon.
    #[must_use]
    pub fn new(
        supervisor: Arc<Supervisor>,
        crafts: Vec<String>,
        ready: Arc<AtomicBool>,
        endpoints: usize,
        telemetry: Arc<Applied>,
    ) -> Self {
        Self {
            supervisor,
            crafts,
            ready,
            endpoints,
            started_at: Instant::now(),
            reload_gate: Mutex::new(()),
            telemetry,
        }
    }

    /// The readiness flag shared with the serve loop.
    #[must_use]
    pub fn ready(&self) -> Arc<AtomicBool> {
        self.ready.clone()
    }
}

/// A running admin listener.
pub struct AdminHandle {
    task: Option<tokio::task::JoinHandle<()>>,
    /// The socket file to remove on stop, when the target is one.
    socket: Option<PathBuf>,
}

impl AdminHandle {
    /// Stop accepting, and remove the socket file if there is one.
    pub async fn stop(mut self) {
        if let Some(task) = self.task.take() {
            task.abort();
            let _ = task.await;
        }
        if let Some(socket) = &self.socket {
            let _ = std::fs::remove_file(socket);
        }
    }
}

/// How this listener learns who is calling.
#[derive(Clone)]
enum Guard {
    /// The kernel names the peer, and it already said yes: only this uid or root
    /// can reach the socket. Nothing to check per request.
    #[cfg(unix)]
    Peer,
    /// The request must carry `Authorization: Bearer <token>`.
    Token(String),
}

/// Bind the admin target and serve until stopped.
///
/// `tls` is required for a `dns` target and meaningless for a unix socket; the
/// same for `token`. The caller (the daemon) decides that from the host
/// configuration, and `Config::validate` has already refused the combinations
/// that would serve an unauthenticated network surface.
pub async fn serve(
    target: &Target,
    tls: Option<tokio_rustls::TlsAcceptor>,
    token: Option<String>,
    state: AdminState,
) -> Result<AdminHandle> {
    let state = Arc::new(state);
    match target {
        #[cfg(unix)]
        Target::Unix(path) => {
            use std::os::unix::fs::PermissionsExt as _;

            if let Some(parent) = path.parent() {
                std::fs::create_dir_all(parent)?;
                std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))?;
            }
            if path.exists() {
                std::fs::remove_file(path)?;
            }
            let listener = tokio::net::UnixListener::bind(path).map_err(|error| {
                Error::not_available(format!("admin socket {}: {error}", path.display()))
            })?;
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;

            let uid = nix::unistd::geteuid().as_raw();
            let task = tokio::spawn(async move {
                loop {
                    match listener.accept().await {
                        Ok((stream, _)) => {
                            let allowed = stream
                                .peer_cred()
                                .map(|credentials| peer_allowed(credentials.uid(), uid))
                                .unwrap_or(false);
                            if !allowed {
                                // Still answered, so a caller that was refused
                                // learns why instead of seeing a closed socket.
                                let _ = write_refusal(stream).await;
                                continue;
                            }
                            let state = state.clone();
                            tokio::spawn(serve_connection(stream, Guard::Peer, state));
                        }
                        Err(failure) => {
                            logerr!(
                                warn,
                                flowcraft::error::Error::from(failure),
                                "admin accept failed"
                            );
                            break;
                        }
                    }
                }
            });
            Ok(AdminHandle {
                task: Some(task),
                socket: Some(path.clone()),
            })
        }
        #[cfg(not(unix))]
        Target::Unix(path) => Err(Error::not_available(format!(
            "admin: {} is a unix socket and this host has none; configure a dns target",
            path.display()
        ))),
        Target::Dns(address) => {
            let Some(acceptor) = tls else {
                return Err(Error::validation("admin: a dns target needs tls"));
            };
            let Some(token) = token else {
                return Err(Error::validation("admin: a dns target needs a token"));
            };
            let listener = std::net::TcpListener::bind(address).map_err(|error| {
                Error::not_available(format!("admin listener {address}: {error}"))
            })?;
            listener.set_nonblocking(true)?;
            let listener = tokio::net::TcpListener::from_std(listener)?;

            let task = tokio::spawn(async move {
                loop {
                    match listener.accept().await {
                        Ok((stream, _)) => {
                            let acceptor = acceptor.clone();
                            let state = state.clone();
                            let guard = Guard::Token(token.clone());
                            tokio::spawn(async move {
                                match acceptor.accept(stream).await {
                                    Ok(stream) => serve_connection(stream, guard, state).await,
                                    Err(failure) => {
                                        logerr!(
                                            warn,
                                            flowcraft::error::Error::from(failure),
                                            "admin tls handshake failed"
                                        );
                                    }
                                }
                            });
                        }
                        Err(failure) => {
                            logerr!(
                                warn,
                                flowcraft::error::Error::from(failure),
                                "admin accept failed"
                            );
                            break;
                        }
                    }
                }
            });
            Ok(AdminHandle {
                task: Some(task),
                socket: None,
            })
        }
    }
}

/// Only the daemon's own uid (or root) may drive the admin socket.
#[cfg(unix)]
fn peer_allowed(peer_uid: u32, self_uid: u32) -> bool {
    peer_uid == self_uid || peer_uid == 0
}

/// A refusal has to be answerable, not a silent close.
#[cfg(unix)]
async fn write_refusal(mut stream: tokio::net::UnixStream) {
    use tokio::io::AsyncWriteExt as _;

    let _ = stream
        .write_all(b"HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
        .await;
    let _ = stream.shutdown().await;
}

impl Guard {
    /// Whether this connection may issue a command.
    fn allows(&self, presented: Option<&str>) -> bool {
        match self {
            #[cfg(unix)]
            Self::Peer => true,
            Self::Token(expected) => bearer_matches(presented, expected),
        }
    }
}

/// Serve HTTP/1.1 on one connection.
async fn serve_connection<I>(io: I, guard: Guard, state: Arc<AdminState>)
where
    I: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send + 'static,
{
    let service = service_fn(move |request: Request<hyper::body::Incoming>| {
        let state = state.clone();
        let guard = guard.clone();
        async move { answer(request, &guard, &state).await }
    });
    if let Err(failure) = hyper::server::conn::http1::Builder::new()
        .serve_connection(TokioIo::new(io), service)
        .await
    {
        logerr!(
            warn,
            flowcraft::error::Error::internal(format!("admin connection: {failure}")),
            "admin connection failed"
        );
    }
}

/// One request: authenticate, map the verb, dispatch, answer.
async fn answer(
    request: Request<hyper::body::Incoming>,
    guard: &Guard,
    state: &AdminState,
) -> std::result::Result<Response<Full<Bytes>>, std::convert::Infallible> {
    let method = request.method().clone();
    let path = request.uri().path().to_owned();
    let presented = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned);

    // Authenticate before the body is touched, and route before it is read:
    // `collect()` on an unbounded body is an allocation primitive, and this
    // surface answers callers that have not proven anything yet. Both checks
    // below are header-only, so a refused request costs a header parse.
    if !guard.allows(presented.as_deref()) {
        return Ok(reply(
            StatusCode::UNAUTHORIZED,
            json!({"ok": false, "error": "admin: missing or wrong bearer token"}),
        ));
    }

    let Some(command) = route(method.as_str(), &path) else {
        let known = [
            "/status", "/reload", "/drain", "/crafts", "/diag", "/revoke",
        ];
        let status = if known.contains(&path.as_str()) {
            StatusCode::METHOD_NOT_ALLOWED
        } else {
            StatusCode::NOT_FOUND
        };
        return Ok(reply(
            status,
            json!({"ok": false, "error": format!("{method} {path} is not an admin route")}),
        ));
    };

    let mut command_request = json!({ "command": command });
    if command == "revoke" {
        // The only verb with an argument, and the only one whose body is read
        // at all: `revoke` takes one session id.
        let body = match read_limited(request.into_body()).await {
            Ok(body) => body,
            Err(status) => {
                return Ok(reply(
                    status,
                    json!({
                        "ok": false,
                        "error": format!("admin: request body refused ({status})"),
                    }),
                ));
            }
        };
        let parsed: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
        if let Some(session) = parsed.get("session") {
            command_request["session"] = session.clone();
        }
    }

    // One span per command: the audit record and everything the command causes
    // share a trace, so "who asked for the reload" and "what the reload did"
    // stop being two unrelated stories.
    let span = tracing::info_span!("admin", flowcraft.command = command);
    let response = dispatch(&command_request, state).instrument(span).await;
    Ok(reply(StatusCode::OK, response))
}

/// The most of a request body the admin surface will read.
///
/// `revoke` sends one session id, so this is generous by three orders of
/// magnitude. The bound exists because a request that is refused *before* it is
/// authenticated must not be able to make the daemon allocate: an unbounded
/// `collect()` is an OOM primitive reachable without a token.
const MAX_ADMIN_BODY: usize = 64 * 1024;

/// Read one body under [`MAX_ADMIN_BODY`].
///
/// Returned as a status because the caller turns it into the same JSON shape as
/// every other refusal; an oversized body is `413`, a broken one `400`.
async fn read_limited<B>(body: B) -> std::result::Result<Bytes, StatusCode>
where
    B: hyper::body::Body<Data = Bytes> + Send + 'static,
    B::Error: Into<Box<dyn std::error::Error + Send + Sync>>,
{
    match Limited::new(body, MAX_ADMIN_BODY).collect().await {
        Ok(collected) => Ok(collected.to_bytes()),
        Err(error) if error.is::<LengthLimitError>() => Err(StatusCode::PAYLOAD_TOO_LARGE),
        Err(_) => Err(StatusCode::BAD_REQUEST),
    }
}

/// The verb a method and path name, or `None` when the path is not one.
fn route(method: &str, path: &str) -> Option<&'static str> {
    match (method, path) {
        ("GET", "/status") => Some("status"),
        ("POST", "/reload") => Some("reload"),
        ("POST", "/drain") => Some("drain"),
        ("GET", "/crafts") => Some("crafts"),
        ("GET", "/diag") => Some("diag"),
        ("POST", "/revoke") => Some("revoke"),
        _ => None,
    }
}

fn reply(status: StatusCode, body: Value) -> Response<Full<Bytes>> {
    let text = serde_json::to_string(&body).unwrap_or_else(|_| "{\"ok\":false}".to_owned());
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Full::new(Bytes::from(text)))
        .unwrap_or_else(|_| Response::new(Full::new(Bytes::new())))
}

/// `Authorization: Bearer <token>`, compared without early exit.
fn bearer_matches(presented: Option<&str>, expected: &str) -> bool {
    let Some(presented) = presented.and_then(|value| value.strip_prefix("Bearer ")) else {
        return false;
    };
    let (presented, expected) = (presented.as_bytes(), expected.as_bytes());
    if presented.len() != expected.len() {
        return false;
    }
    presented
        .iter()
        .zip(expected)
        .fold(0u8, |difference, (a, b)| difference | (a ^ b))
        == 0
}

async fn dispatch(request: &Value, state: &AdminState) -> Value {
    let command = request.get("command").and_then(|value| value.as_str());
    tracing::info!(
        flowcraft.command = command.unwrap_or_default(),
        "admin command"
    );
    match command {
        Some("status") => {
            let generation = state.supervisor.generation().await;
            let entries = state.supervisor.entries().await;
            // The bounds each runner enforces, next to what it is holding: a
            // limit nobody can read is a limit nobody can plan against.
            let runners: Vec<Value> = state
                .supervisor
                .sandbox_stats()
                .await
                .into_iter()
                .map(|(entry, stats)| {
                    json!({
                        "entry": entry,
                        "sessions": {
                            "ready": stats.sessions_ready,
                            "exited": stats.sessions_exited,
                        },
                        "log_bytes": stats.log_bytes,
                    })
                })
                .collect();
            json!({
                "ok": true,
                "ready": state.ready.load(Ordering::SeqCst),
                "generation": generation,
                "entries": entries,
                "sandbox": {
                    "runners": runners,
                    // Pruned by expiry, so this should plateau: a number that
                    // climbs without bound is the symptom of a pruning bug.
                    "grants_revoked": state.supervisor.platform().revoked_grants(),
                },
                "crafts": state.crafts,
                "endpoints": state.endpoints,
                "uptime_secs": state.started_at.elapsed().as_secs(),
            })
        }
        Some("reload") => {
            let _gate = state.reload_gate.lock().await;
            match state.supervisor.reload().await {
                Ok(outcome) => json!({
                    "ok": true,
                    "previous": outcome.previous,
                    "current": outcome.current,
                    "resources": outcome.resources,
                    "drained": outcome.drained,
                }),
                Err(error) => json!({"ok": false, "error": error.to_string()}),
            }
        }
        Some("drain") => {
            // Readiness off, and no new leases: existing work finishes, new
            // sessions cannot start.
            state.ready.store(false, Ordering::SeqCst);
            state.supervisor.platform().drain();
            json!({"ok": true, "ready": false})
        }
        Some("crafts") => json!({"ok": true, "crafts": state.crafts}),
        Some("diag") => {
            // Everything a support bundle needs about this process, except
            // anything that is itself a credential: the identity salt and any
            // token stay here.
            let resource = &state.telemetry.install.resource;
            json!({
                "ok": true,
                "service": resource.service,
                "version": resource.version,
                "install": resource.install,
                "install_scope": resource.install_scope,
                "install_epoch": resource.install_epoch,
                "principal": resource.principal,
                "session": resource.session,
                "format": match state.telemetry.install.format {
                    flowcraft::telemetry::Format::Text => "text",
                    flowcraft::telemetry::Format::Json => "json",
                },
                "profile": format!("{:?}", state.telemetry.install.profile),
                "directives": state.telemetry.guard.directives(),
                "file": state.telemetry.guard.file_path().map(|path| path.display().to_string()),
                "file_disabled": state.telemetry.guard.file_disabled(),
                "ring_bytes": state.telemetry.guard.ring_len(),
                "ring": state.telemetry.guard.ring_dump(),
            })
        }
        Some("revoke") => {
            let session = request
                .get("session")
                .and_then(|value| value.as_str())
                .unwrap_or_default();
            if session.is_empty() {
                return json!({"ok": false, "error": "revoke needs a session"});
            }
            match state.supervisor.platform().revoke_session(session) {
                Ok(()) => json!({"ok": true, "session": session}),
                Err(error) => json!({"ok": false, "error": error.to_string()}),
            }
        }
        Some(other) => json!({"ok": false, "error": format!("unknown command {other:?}")}),
        None => json!({"ok": false, "error": "command is required"}),
    }
}

/// Build the TLS acceptor a `dns` admin target serves with.
///
/// Certificate and key arrive as resolved bytes, not references: the caller read
/// them through the bootstrap provider, the same way endpoint material is read.
pub fn acceptor(cert: &[u8], key: &[u8]) -> Result<tokio_rustls::TlsAcceptor> {
    use rustls_pki_types::pem::PemObject as _;
    use rustls_pki_types::{CertificateDer, PrivateKeyDer};

    let certificates = CertificateDer::pem_slice_iter(cert)
        .collect::<std::result::Result<Vec<_>, _>>()
        .map_err(|error| Error::validation(format!("admin tls certificate: {error}")))?;
    if certificates.is_empty() {
        return Err(Error::validation("admin tls certificate: no PEM section"));
    }
    let key = PrivateKeyDer::from_pem_slice(key)
        .map_err(|error| Error::validation(format!("admin tls key: {error}")))?;

    crate::kernel_tls_provider();
    let config = rustls::ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(certificates, key)
        .map_err(|error| Error::validation(format!("admin tls: {error}")))?;
    Ok(tokio_rustls::TlsAcceptor::from(std::sync::Arc::new(config)))
}

/// A fresh bearer token: 32 bytes of randomness, hex.
pub fn mint_token() -> String {
    format!("{:032x}", rand::random::<u128>())
}

/// Write the token where `ctl` will look for it, owner-only on unix.
///
/// The mode is part of the `open`, not a `set_permissions` afterwards: this file
/// is a bearer credential for the admin socket, and a create-then-chmod leaves a
/// window in which it exists with the process umask. `create_new` also means a
/// stale token from an earlier run is replaced deliberately (see below) rather
/// than raced.
pub fn write_token(path: &std::path::Path, token: &str) -> Result<()> {
    use std::io::Write as _;

    if let Some(parent) = path.parent()
        && !parent.as_os_str().is_empty()
    {
        flowcraft::fs::create_private_dir(parent)?;
    }
    // A previous run's token is useless from here on: remove it first, so the
    // exclusive create below cannot fail on a leftover file.
    match std::fs::remove_file(path) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => {
            return Err(Error::internal(format!(
                "admin token file {path:?}: {error}"
            )));
        }
    }
    let mut file = flowcraft::fs::private_file(path)?;
    file.write_all(format!("{token}\n").as_bytes())?;
    file.sync_all()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_routes_are_the_six_verbs() {
        assert_eq!(route("GET", "/status"), Some("status"));
        assert_eq!(route("POST", "/reload"), Some("reload"));
        assert_eq!(route("POST", "/drain"), Some("drain"));
        assert_eq!(route("GET", "/crafts"), Some("crafts"));
        assert_eq!(route("GET", "/diag"), Some("diag"));
        assert_eq!(route("POST", "/revoke"), Some("revoke"));
        // A read is not a write, and an unknown path is not a command.
        assert_eq!(route("POST", "/status"), None);
        assert_eq!(route("GET", "/reload"), None);
        assert_eq!(route("GET", "/nope"), None);
    }

    #[test]
    fn a_bearer_token_is_compared_exactly() {
        assert!(bearer_matches(Some("Bearer abc"), "abc"));
        assert!(!bearer_matches(Some("Bearer abX"), "abc"));
        assert!(!bearer_matches(Some("Bearer abc "), "abc"));
        assert!(!bearer_matches(Some("abc"), "abc"));
        assert!(!bearer_matches(None, "abc"));
        assert!(!bearer_matches(Some("Bearer "), "abc"));
    }

    #[cfg(unix)]
    #[test]
    fn only_the_owner_or_root_may_use_the_socket() {
        assert!(peer_allowed(1000, 1000));
        assert!(peer_allowed(0, 1000));
        assert!(!peer_allowed(1001, 1000));
    }

    /// The bound is what makes an unauthenticated caller unable to make the
    /// daemon allocate: without it, `collect()` buffers whatever arrives.
    #[tokio::test]
    async fn a_body_over_the_bound_is_refused() {
        let body = Full::new(Bytes::from(vec![b'x'; MAX_ADMIN_BODY + 1]));
        assert_eq!(
            read_limited(body).await.expect_err("oversized"),
            StatusCode::PAYLOAD_TOO_LARGE
        );
    }

    #[tokio::test]
    async fn a_body_within_the_bound_is_returned_whole() {
        let body = Full::new(Bytes::from_static(b"{\"session\":\"s-1\"}"));
        assert_eq!(
            read_limited(body).await.expect("read"),
            Bytes::from_static(b"{\"session\":\"s-1\"}")
        );
    }
}
