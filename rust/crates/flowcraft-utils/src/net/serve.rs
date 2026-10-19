//! The serving half: one listener, two protocols, and the decisions between
//! them.
//!
//! Core's proxy binds a unix socket (or an ephemeral loopback port) and peeks
//! the first byte of every connection: `0x05` is a SOCKS5 session, anything
//! else is HTTP. One address serves both because the sandbox only gets one --
//! the bwrap bridge mounts a single socket, and the seatbelt profile allows a
//! single loopback port.
//!
//! # What this module owns, and what it does not
//!
//! It owns the classification, the listener, the audit and hook wiring, and the
//! three refusals a client can see before a tunnel exists. It does **not** parse
//! HTTP: core hands that to `net/http`, and the port hands it to whatever
//! [`HttpHandler`] the caller supplies (hyper, in the slice that lands the
//! forward path). [`Server::handle_connect`] is the piece such a handler calls
//! once it has parsed a `CONNECT` request -- the decision, the audit, the hook,
//! the `200`, the dial and the pump, in core's order.
//!
//! # The refusals
//!
//! Three bodies reach an HTTP client, and their wording is what a user reads
//! when something is blocked:
//!
//! | when | status | body |
//! | --- | --- | --- |
//! | the policy could not judge the destination | 502 | `netproxy: <the reason>` |
//! | a deny rule decided | 403 | `netproxy: destination not allowed` (+ `: <reason>`) |
//! | a hook blocked the connection | 403 | `netproxy: connect blocked by hook` |
//!
//! A SOCKS5 client gets none of that: the protocol has one failure code, so
//! every refusal is `0x01` and the reason lives in the audit trail only.
//!
//! # The audit trail
//!
//! Core writes one record per decision, and it does so at slightly different
//! points on the two paths: the SOCKS5 path records the deny it serves when the
//! policy could not judge a destination, and the HTTP paths drop that record
//! on the floor. The port keeps both, because a caller that migrates should see
//! the trail it saw before; the record it does emit is
//! [`Audit`][super::proxy::Audit], which is core's `ProxyDecision`.

use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};
use tokio_util::sync::CancellationToken;

use super::BoxFuture;
use super::dial::{self, Dialer};
use super::policy::NetAction;
use super::proxy::{Audit, Proxy, deny_reason_suffix};
use super::socks5;

/// The line a hijacked `CONNECT` gets once the policy allowed it.
pub const CONNECT_ESTABLISHED: &[u8] = b"HTTP/1.1 200 Connection Established\r\n\r\n";

/// Which protocol a connection speaks, decided by its first byte.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Route {
    /// A SOCKS5 session.
    Socks5,
    /// Anything else: HTTP, which is somebody else's parser.
    Http,
}

/// The mux: core's `b[0] == socks5Version` test.
pub fn classify(first: u8) -> Route {
    if first == socks5::VERSION {
        Route::Socks5
    } else {
        Route::Http
    }
}

/// One HTTP refusal, in the shape `http.Error` writes it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Refusal {
    /// The status code.
    pub status: u16,
    /// The body, newline included.
    pub body: String,
}

impl Refusal {
    /// The policy could not judge the destination: core answers `502` and puts
    /// the reason in the body.
    pub fn unjudged(reason: &str) -> Refusal {
        Refusal {
            status: 502,
            body: format!("netproxy: {reason}\n"),
        }
    }

    /// A deny rule decided, with its model-facing explanation.
    pub fn denied(reason: &str) -> Refusal {
        Refusal {
            status: 403,
            body: format!("netproxy: destination not allowed{}\n", deny_reason_suffix(reason)),
        }
    }

    /// A hook blocked the connection.
    pub fn blocked() -> Refusal {
        Refusal {
            status: 403,
            body: "netproxy: connect blocked by hook\n".to_owned(),
        }
    }

    /// The canonical reason phrase for [`Self::status`], for a caller writing
    /// the status line itself.
    pub fn status_text(&self) -> &'static str {
        match self.status {
            403 => "Forbidden",
            502 => "Bad Gateway",
            _ => "",
        }
    }
}

/// Where decision records go. Core records one per decision through a
/// `func(ProxyDecision)`, which this is the trait form of.
pub trait AuditSink: Send + Sync {
    /// Record one decision.
    fn record(&self, audit: &Audit);
}

impl<F: Fn(&Audit) + Send + Sync> AuditSink for F {
    fn record(&self, audit: &Audit) {
        self(audit);
    }
}

/// The hooks core's `MITMHooks` exposes before TLS termination: `OnConnect`
/// applies to every `CONNECT` (HTTP and SOCKS5 alike), and a refusal blocks the
/// connection.
pub trait Hooks: Send + Sync {
    /// Called once the policy allowed a destination, before it is dialed.
    fn on_connect(&self, host: &str, port: i32) -> Result<(), String>;
}

/// What [`Server::handle_connect`] produced.
#[derive(Debug)]
pub enum Outcome {
    /// Refused before any tunnel existed: the caller writes this response.
    Refused(Refusal),
    /// The `200` went out and the tunnel ran until one side closed.
    Established,
    /// The `200` went out, and then the target could not be dialed. Core drops
    /// this error silently; the port reports it to the caller so it can be
    /// logged, which is the only difference the tunnel has.
    DialFailed(dial::Error),
}

/// What the decision and the hook produced for one destination.
struct Judgment {
    /// The `host:port` to dial.
    target: String,
    /// The destination as the request spelled it.
    host: String,
    /// The destination's port, after the default was applied.
    port: i32,
}

/// The HTTP side of the mux: whatever parses HTTP, given the connection after
/// classification.
pub trait HttpHandler: Send + Sync {
    /// Handle one HTTP connection to completion.
    fn handle<'a>(&'a self, connection: Box<dyn socks5::Stream>) -> BoxFuture<'a, ()>;
}
impl<F> HttpHandler for F
where
    F: Fn(Box<dyn socks5::Stream>) -> BoxFuture<'static, ()> + Send + Sync,
{
    fn handle<'a>(&'a self, connection: Box<dyn socks5::Stream>) -> BoxFuture<'a, ()> {
        self(connection)
    }
}

/// The serving proxy: a policy, a dialer, the audit sink, the hooks, and the
/// HTTP handler.
pub struct Server {
    proxy: Proxy,
    dialer: Dialer,
    audit: Option<Arc<dyn AuditSink>>,
    hooks: Option<Arc<dyn Hooks>>,
    http: Arc<dyn HttpHandler>,
    socks5: socks5::Server<PolicyConnector>,
    handshake_timeout: Duration,
    dial_timeout: Duration,
}

impl std::fmt::Debug for Server {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Server")
            .field("proxy", &self.proxy)
            .field("dialer", &self.dialer)
            .field("audit", &self.audit.is_some())
            .field("hooks", &self.hooks.is_some())
            .finish_non_exhaustive()
    }
}

impl Server {
    /// A server over a policy and a dialer, with no audit sink and no hooks.
    pub fn new(proxy: Proxy, dialer: Dialer, http: Arc<dyn HttpHandler>) -> Server {
        let connector = PolicyConnector {
            proxy: proxy.clone(),
            dialer: dialer.clone(),
            audit: None,
            hooks: None,
        };
        Server {
            proxy,
            dialer,
            audit: None,
            hooks: None,
            http,
            socks5: socks5::Server::new(connector),
            handshake_timeout: socks5::HANDSHAKE_TIMEOUT,
            dial_timeout: socks5::DIAL_TIMEOUT,
        }
    }

    /// Attach the audit sink, so every decision is recorded once.
    pub fn with_audit(mut self, audit: Arc<dyn AuditSink>) -> Server {
        self.audit = Some(audit);
        self.rebuild_connector();
        self
    }

    /// Attach the hooks, which see every allowed destination before the dial.
    pub fn with_hooks(mut self, hooks: Arc<dyn Hooks>) -> Server {
        self.hooks = Some(hooks);
        self.rebuild_connector();
        self
    }

    /// Attach the HTTP handler (hyper, or a test's stub).
    pub fn with_http(mut self, http: Arc<dyn HttpHandler>) -> Server {
        self.http = http;
        self
    }

    /// The policy object this server decides with.
    pub fn proxy(&self) -> &Proxy {
        &self.proxy
    }

    fn rebuild_connector(&mut self) {
        let connector = PolicyConnector {
            proxy: self.proxy.clone(),
            dialer: self.dialer.clone(),
            audit: self.audit.clone(),
            hooks: self.hooks.clone(),
        };
        let server = socks5::Server::new(connector)
            .with_timeouts(self.handshake_timeout, self.dial_timeout);
        self.socks5 = server;
    }

    /// Serve one classified connection. The connection is dropped when this
    /// returns.
    pub async fn handle(&self, connection: Box<dyn socks5::Stream>) {
        let mut reader = BufReader::new(connection);
        let first = match reader.fill_buf().await {
            Ok(buffer) if !buffer.is_empty() => buffer[0],
            _ => return,
        };
        match classify(first) {
            Route::Socks5 => self.socks5.serve(reader).await,
            Route::Http => self.http.handle(Box::new(reader)).await,
        }
    }

    /// Judge one destination and record it, exactly as core does before it acts.
    fn judge(&self, hostport: &str, default_port: i32) -> Result<Judgment, Refusal> {
        let verdict = self.proxy.decide(hostport, default_port);
        // Core's HTTP paths refuse an unjudgeable destination before they audit
        // it; its SOCKS5 path audits first (see `PolicyConnector`).
        if let Some(error) = &verdict.error {
            return Err(Refusal::unjudged(&error.to_string()));
        }
        if let Some(audit) = &self.audit {
            audit.record(&verdict.record);
        }
        if verdict.record.action != NetAction::Allow {
            return Err(Refusal::denied(&verdict.record.reason));
        }
        Ok(Judgment {
            target: verdict.record.dial_host,
            host: verdict.record.host,
            port: verdict.record.port,
        })
    }

    /// The `CONNECT` path: decide, audit, hook, answer `200`, dial, and pump.
    ///
    /// This is what an HTTP handler calls once it has a `CONNECT` request and
    /// the hijacked connection. `hostport` is the request's authority.
    pub async fn handle_connect<S>(&self, client: &mut S, hostport: &str) -> Outcome
    where
        S: AsyncRead + AsyncWrite + Unpin,
    {
        let judgment = match self.judge(hostport, 443) {
            Ok(judgment) => judgment,
            Err(refusal) => return Outcome::Refused(refusal),
        };
        if let Some(hooks) = &self.hooks
            && hooks.on_connect(&judgment.host, judgment.port).is_err()
        {
            return Outcome::Refused(Refusal::blocked());
        }
        if client.write_all(CONNECT_ESTABLISHED).await.is_err() {
            return Outcome::DialFailed(dial::Error::Io(io::Error::other(
                "client vanished before the tunnel",
            )));
        }
        match self.dialer.dial(&judgment.target).await {
            Ok(dialed) => {
                let (mut client_read, mut client_write) = tokio::io::split(client);
                let (mut target_read, mut target_write) = tokio::io::split(dialed.stream);
                socks5::pump(
                    &mut client_read,
                    &mut client_write,
                    &mut target_read,
                    &mut target_write,
                )
                .await;
                Outcome::Established
            }
            Err(err) => Outcome::DialFailed(err),
        }
    }
}

/// The SOCKS5 connector: decide, audit, hook, then dial.
///
/// The protocol has one failure code, so every refusal here is an error the
/// session turns into `0x01`.
struct PolicyConnector {
    proxy: Proxy,
    dialer: Dialer,
    audit: Option<Arc<dyn AuditSink>>,
    hooks: Option<Arc<dyn Hooks>>,
}

impl socks5::Connector for PolicyConnector {
    fn connect<'a>(&'a self, target: &'a socks5::Target) -> BoxFuture<'a, io::Result<socks5::Dialed>> {
        Box::pin(async move {
            let hostport = target.host_port();
            let verdict = self.proxy.decide(&hostport, 0);
            if let Some(audit) = &self.audit {
                // The SOCKS5 path records the deny it is about to serve, which
                // is the one place core audits a destination it could not
                // judge.
                audit.record(&verdict.record);
            }
            if let Some(error) = &verdict.error {
                return Err(io::Error::other(error.to_string()));
            }
            if verdict.record.action != NetAction::Allow {
                return Err(io::Error::other(format!(
                    "netproxy: destination not allowed{}",
                    deny_reason_suffix(&verdict.record.reason)
                )));
            }
            if let Some(hooks) = &self.hooks
                && hooks
                    .on_connect(&verdict.record.host, verdict.record.port)
                    .is_err()
            {
                return Err(io::Error::other("netproxy: connect blocked by hook"));
            }
            self.dialer
                .dial(&verdict.record.dial_host)
                .await
                .map_err(|err| io::Error::other(err.to_string()))
        })
    }
}

/// A listener this crate bound, and the cleanup it owes.
pub struct Bound {
    listener: Listener,
    server: Arc<Server>,
    /// The temp directory a unix socket lives in, removed on shutdown.
    place: Option<PathBuf>,
    /// The socket path, when this is a unix listener.
    path: Option<PathBuf>,
    cancel: CancellationToken,
}

enum Listener {
    Tcp(tokio::net::TcpListener),
    #[cfg(unix)]
    Unix(tokio::net::UnixListener),
}

impl Listener {
    async fn accept(&self) -> io::Result<Box<dyn socks5::Stream>> {
        match self {
            Listener::Tcp(listener) => {
                let (stream, _) = listener.accept().await?;
                Ok(Box::new(stream))
            }
            #[cfg(unix)]
            Listener::Unix(listener) => {
                let (stream, _) = listener.accept().await?;
                Ok(Box::new(stream))
            }
        }
    }
}

impl Bound {
    /// The bound address, for a TCP listener.
    pub fn addr(&self) -> Option<SocketAddr> {
        match &self.listener {
            Listener::Tcp(listener) => listener.local_addr().ok(),
            #[cfg(unix)]
            Listener::Unix(_) => None,
        }
    }

    /// The socket path, for a unix listener.
    pub fn path(&self) -> Option<&Path> {
        self.path.as_deref()
    }

    /// Accept until [`Self::shutdown`] is called, serving each connection on
    /// its own task.
    pub async fn serve(&self) {
        loop {
            let accepted = tokio::select! {
                _ = self.cancel.cancelled() => break,
                accepted = self.listener.accept() => accepted,
            };
            match accepted {
                Ok(connection) => {
                    let server = Arc::clone(&self.server);
                    tokio::spawn(async move { server.handle(connection).await });
                }
                // Core's accept loop stops on the first error, because the only
                // error it can see is the listener closing.
                Err(_) => break,
            }
        }
    }

    /// Stop accepting and remove the temp directory a unix socket lives in.
    pub fn shutdown(&self) {
        self.cancel.cancel();
        self.remove_socket();
    }

    fn remove_socket(&self) {
        if let Some(place) = &self.place {
            let _ = std::fs::remove_dir_all(place);
        }
    }
}

impl Drop for Bound {
    fn drop(&mut self) {
        self.cancel.cancel();
        self.remove_socket();
    }
}

/// Bind an ephemeral loopback port. The seatbelt backend uses this, because its
/// child shares the host network stack and reaches the proxy through the single
/// allowed port.
pub async fn bind_tcp(server: Server) -> io::Result<Bound> {
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0)).await?;
    Ok(Bound {
        listener: Listener::Tcp(listener),
        server: Arc::new(server),
        place: None,
        path: None,
        cancel: CancellationToken::new(),
    })
}

/// Bind a unix socket in a fresh per-run temp directory, mode `0600`.
///
/// The directory is mode `0700` and is removed when the bound listener is shut
/// down or dropped, so a crashed proxy leaves nothing behind that a next run
/// would trip over.
#[cfg(unix)]
pub async fn bind_unix(server: Server) -> io::Result<Bound> {
    use std::os::unix::fs::PermissionsExt;

    let place = unique_temp_dir()?;
    std::fs::set_permissions(&place, std::fs::Permissions::from_mode(0o700))?;
    let path = place.join("proxy.sock");
    let listener = match tokio::net::UnixListener::bind(&path) {
        Ok(listener) => listener,
        Err(err) => {
            let _ = std::fs::remove_dir_all(&place);
            return Err(err);
        }
    };
    if let Err(err) = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)) {
        let _ = std::fs::remove_dir_all(&place);
        return Err(err);
    }
    Ok(Bound {
        listener: Listener::Unix(listener),
        server: Arc::new(server),
        place: Some(place),
        path: Some(path),
        cancel: CancellationToken::new(),
    })
}

/// A temp directory nobody else will pick, named the way core names its own.
#[cfg(unix)]
fn unique_temp_dir() -> io::Result<PathBuf> {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|elapsed| elapsed.subsec_nanos())
        .unwrap_or_default();
    let name = format!("flowcraft-netproxy-{}-{nanos}", std::process::id());
    let place = std::env::temp_dir().join(name);
    std::fs::create_dir(&place)?;
    Ok(place)
}


#[cfg(test)]
mod tests {
    use super::*;
    use crate::net::policy::{NetMode, NetRule};
    use crate::net::proxy::ProxyConfig;
    use crate::net::socks5;
    use std::sync::Mutex;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    fn ok<T, E: std::fmt::Debug>(result: Result<T, E>, what: &str) -> T {
        match result {
            Ok(value) => value,
            Err(err) => panic!("{what}: {err:?}"),
        }
    }

    fn rule(action: NetAction, host: &str, port: i32) -> NetRule {
        NetRule {
            action,
            host: host.to_owned(),
            port,
            ..NetRule::default()
        }
    }

    /// A server over one policy, with the decisions kept for inspection.
    fn server(rules: Vec<NetRule>, mode: NetMode) -> (Server, Arc<Mutex<Vec<Audit>>>) {
        let config = ProxyConfig {
            mode,
            rules,
            ..ProxyConfig::default()
        };
        let proxy = Proxy::new(&config);
        let seen = Arc::new(Mutex::new(Vec::new()));
        let recorder = Arc::clone(&seen);
        let sink: Arc<dyn AuditSink> = Arc::new(move |record: &Audit| {
            if let Ok(mut log) = recorder.lock() {
                log.push(record.clone());
            }
        });
        let http: Arc<dyn HttpHandler> = Arc::new(
            |_connection: Box<dyn socks5::Stream>| -> BoxFuture<'static, ()> { Box::pin(async {}) },
        );
        (
            Server::new(proxy, Dialer::direct(), http).with_audit(sink),
            seen,
        )
    }

    /// A loopback echo server standing in for an allowed destination.
    async fn echo() -> SocketAddr {
        let listener = ok(
            tokio::net::TcpListener::bind(("127.0.0.1", 0)).await,
            "bind echo",
        );
        let address = ok(listener.local_addr(), "echo address");
        tokio::spawn(async move {
            loop {
                let Ok((mut stream, _)) = listener.accept().await else {
                    return;
                };
                tokio::spawn(async move {
                    let (mut read, mut write) = stream.split();
                    let _ = tokio::io::copy(&mut read, &mut write).await;
                });
            }
        });
        address
    }

    #[test]
    fn the_first_byte_decides_the_protocol() {
        assert_eq!(classify(socks5::VERSION), Route::Socks5);
        for byte in [0x00u8, b'G', b'P', b'C', 0x04, 0x06, 0xff] {
            assert_eq!(classify(byte), Route::Http, "{byte:#04x}");
        }
    }

    #[test]
    fn the_refusals_are_the_words_a_user_reads() {
        assert_eq!(
            Refusal::denied(""),
            Refusal {
                status: 403,
                body: "netproxy: destination not allowed\n".to_owned()
            }
        );
        assert_eq!(
            Refusal::denied("blocked for the demo"),
            Refusal {
                status: 403,
                body: "netproxy: destination not allowed: blocked for the demo\n".to_owned()
            }
        );
        assert_eq!(
            Refusal::unjudged("netproxy: policy matcher unavailable"),
            Refusal {
                status: 502,
                body: "netproxy: netproxy: policy matcher unavailable\n".to_owned()
            }
        );
        assert_eq!(
            Refusal::blocked().body,
            "netproxy: connect blocked by hook\n"
        );
        assert_eq!(Refusal::denied("").status_text(), "Forbidden");
        assert_eq!(Refusal::unjudged("x").status_text(), "Bad Gateway");
    }

    #[tokio::test]
    async fn an_allowed_connect_answers_then_tunnels() {
        let target = echo().await;
        let (service, mut client) = tokio::io::duplex(64 * 1024);
        let (server, seen) = server(
            vec![rule(NetAction::Allow, "127.0.0.1", i32::from(target.port()))],
            NetMode::AllowList,
        );
        let authority = target.to_string();
        let task = tokio::spawn(async move {
            let mut service = service;
            server.handle_connect(&mut service, &authority).await
        });

        let mut line = vec![0u8; CONNECT_ESTABLISHED.len()];
        ok(client.read_exact(&mut line).await, "the 200");
        assert_eq!(line, CONNECT_ESTABLISHED);
        ok(client.write_all(b"ping").await, "write");
        let mut echoed = [0u8; 4];
        ok(client.read_exact(&mut echoed).await, "echo");
        assert_eq!(&echoed, b"ping");
        ok(client.shutdown().await, "half-close");
        assert!(matches!(task.await, Ok(Outcome::Established)));

        let seen = seen.lock().map(|log| log.clone()).unwrap_or_default();
        assert_eq!(seen.len(), 1, "{seen:?}");
        assert_eq!(seen[0].action, NetAction::Allow);
        assert_eq!(seen[0].host, "127.0.0.1");
    }

    #[tokio::test]
    async fn a_denied_connect_is_refused_before_the_line() {
        let (server, seen) = server(
            vec![NetRule {
                action: NetAction::Deny,
                host: "blocked.example".to_owned(),
                port: 0,
                reason: "blocked for the demo".to_owned(),
            }],
            NetMode::AllowList,
        );
        let (mut service, mut client) = tokio::io::duplex(64 * 1024);
        let outcome = server.handle_connect(&mut service, "blocked.example:443").await;
        // The connection is core's to close, and closing it is how the client
        // learns the request is over.
        drop(service);
        match outcome {
            Outcome::Refused(refusal) => {
                assert_eq!(refusal.status, 403);
                assert_eq!(
                    refusal.body,
                    "netproxy: destination not allowed: blocked for the demo\n"
                );
            }
            other => panic!("expected a refusal, got {other:?}"),
        }
        // Nothing reached the client: the refusal is the caller's to write.
        let mut byte = [0u8; 1];
        assert_eq!(ok(client.read(&mut byte).await, "read"), 0);

        let seen = seen.lock().map(|log| log.clone()).unwrap_or_default();
        assert_eq!(seen.len(), 1);
        assert_eq!(seen[0].reason, "blocked for the demo");
    }

    #[tokio::test]
    async fn a_destination_the_policy_cannot_judge_is_a_bad_gateway() {
        // A rule host that does not compile leaves the whole matcher unusable,
        // which is core's "policy matcher unavailable".
        let (server, seen) = server(
            vec![rule(NetAction::Allow, "exa*mple.com", 0)],
            NetMode::AllowList,
        );
        let (mut service, _client) = tokio::io::duplex(64 * 1024);
        match server.handle_connect(&mut service, "example.com:443").await {
            Outcome::Refused(refusal) => {
                assert_eq!(refusal.status, 502);
                assert_eq!(refusal.body, "netproxy: netproxy: policy matcher unavailable\n");
            }
            other => panic!("expected a refusal, got {other:?}"),
        }
        // Core's HTTP paths do not audit a destination they could not judge.
        let seen = seen.lock().map(|log| log.clone()).unwrap_or_default();
        assert!(seen.is_empty(), "{seen:?}");
    }

    struct Refuse;

    impl Hooks for Refuse {
        fn on_connect(&self, _host: &str, _port: i32) -> Result<(), String> {
            Err("not this one".to_owned())
        }
    }

    #[tokio::test]
    async fn a_hook_can_stop_an_allowed_destination() {
        let target = echo().await;
        let (service, _client) = tokio::io::duplex(64 * 1024);
        let (server, _seen) = server(
            vec![rule(NetAction::Allow, "127.0.0.1", i32::from(target.port()))],
            NetMode::AllowList,
        );
        let server = server.with_hooks(Arc::new(Refuse));
        let mut service = service;
        match server
            .handle_connect(&mut service, &target.to_string())
            .await
        {
            Outcome::Refused(refusal) => {
                assert_eq!(refusal.status, 403);
                assert_eq!(refusal.body, "netproxy: connect blocked by hook\n");
            }
            other => panic!("expected a refusal, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn one_listener_serves_socks5_and_http() {
        let target = echo().await;
        let reached = Arc::new(Mutex::new(false));
        let marker = Arc::clone(&reached);
        let http: Arc<dyn HttpHandler> = Arc::new(
            move |mut connection: Box<dyn socks5::Stream>| -> BoxFuture<'static, ()> {
                let marker = Arc::clone(&marker);
                Box::pin(async move {
                    if let Ok(mut seen) = marker.lock() {
                        *seen = true;
                    }
                    let _ = connection.write_all(b"HTTP/1.1 204 No Content\r\n\r\n").await;
                })
            },
        );
        let (server, _seen) = server(
            vec![rule(NetAction::Allow, "127.0.0.1", i32::from(target.port()))],
            NetMode::AllowList,
        );
        let bound = ok(bind_tcp(server.with_http(http)).await, "bind");
        let address = match bound.addr() {
            Some(address) => address,
            None => panic!("a tcp listener has an address"),
        };
        let serving = tokio::spawn(async move {
            bound.serve().await;
        });

        // A SOCKS5 client reaches the same rules an HTTP client would.
        let mut session = ok(tokio::net::TcpStream::connect(address).await, "connect");
        ok(
            socks5::negotiate(&mut session, &target.to_string(), None).await,
            "socks5 negotiate",
        );
        ok(session.write_all(b"ping").await, "socks5 write");
        let mut echoed = [0u8; 4];
        ok(session.read_exact(&mut echoed).await, "socks5 echo");
        assert_eq!(&echoed, b"ping");

        // Anything else is HTTP, and lands in the handler.
        let mut other = ok(tokio::net::TcpStream::connect(address).await, "connect");
        ok(
            other
                .write_all(b"GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
                .await,
            "http write",
        );
        let mut line = [0u8; 15];
        ok(other.read_exact(&mut line).await, "http reply");
        assert_eq!(&line, b"HTTP/1.1 204 No");
        assert!(
            reached.lock().map(|reached| *reached).unwrap_or(false),
            "the HTTP handler never ran"
        );

        serving.abort();
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_unix_listener_lives_in_a_private_temp_directory() {
        use std::os::unix::fs::PermissionsExt;

        let (server, _seen) = server(Vec::new(), NetMode::AllowList);
        let bound = ok(bind_unix(server).await, "bind unix");
        let path = match bound.path() {
            Some(path) => path.to_path_buf(),
            None => panic!("a unix listener has a path"),
        };
        assert!(path.exists(), "{}", path.display());
        let mode = ok(std::fs::metadata(&path), "socket metadata")
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o600, "{:o}", mode);
        let place = path.parent().map(|place| place.to_path_buf());
        bound.shutdown();
        assert!(!path.exists(), "the socket survived shutdown");
        if let Some(place) = place {
            assert!(!place.exists(), "the temp directory survived shutdown");
        }
    }
}
