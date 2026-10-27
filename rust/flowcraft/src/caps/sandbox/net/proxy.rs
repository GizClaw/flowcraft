//! The enforcement proxy: the one place a sandbox can reach the network.
//!
//! One listener serves three protocols on the same port, because a client only
//! gets one hole and does not always speak the same one:
//!
//! - **HTTP `CONNECT`** for tunnels (`https://`, and anything else a client
//!   tunnels);
//! - **absolute-form HTTP** for plain `http://`;
//! - **SOCKS5 `CONNECT`**, which is what `ALL_PROXY` points at so that a
//!   non-HTTP client still goes through the same decision.
//!
//! The proxy is the only component that resolves, dials or decides, and it asks
//! [`Matcher`] rather than deciding itself. Two properties are worth stating:
//!
//! - the target comes from the protocol (the request line, the `Host` header, the
//!   SOCKS5 request), never from an environment variable, so a client cannot
//!   nominate a destination the rules never saw;
//! - the address it dials is the one the matcher pinned, so a second resolution
//!   cannot steer the connection elsewhere.
//!
//! Upstream chaining (`proxy: http://…` / `socks5://…`) is **not implemented**:
//! a policy that asks for it is refused at build time rather than silently
//! dialling direct. `proxy` mode without an upstream is a deny-list proxy, which
//! is what it does today.

use std::io;
use std::net::{IpAddr, SocketAddr};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::task::{Context, Poll};
use std::time::Duration;

use crate::error::{Error, Result};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::task::JoinHandle;
use tokio::time::Instant;

use super::ProxyLimits;
use super::matcher::Matcher;
use crate::caps::sandbox::contract::{NetMode, NetPolicy};

/// How much of a request head is read before giving up.
const MAX_HEAD: usize = 16 * 1024;

/// The variables the sandbox owns while proxying.
pub(crate) const OWNED_ENV: &[&str] = &[
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "ALL_PROXY",
    "NO_PROXY",
    "http_proxy",
    "https_proxy",
    "all_proxy",
    "no_proxy",
];

/// Where the proxy listens.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Listen {
    /// A unix socket, bind-mounted into a network namespace.
    Unix(PathBuf),
    /// A loopback port, for a sandbox that shares the host network stack.
    Loopback,
}

/// Where a started proxy can be reached.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Endpoint {
    /// The unix socket path.
    Unix(PathBuf),
    /// The loopback port, and the credential every client must present.
    Loopback {
        /// The bound port.
        port: u16,
        /// What the listener demands of every connection.
        credential: Credential,
    },
}

impl Endpoint {
    /// The socket path, when this is a unix endpoint.
    pub(crate) fn socket(&self) -> Option<&Path> {
        match self {
            Self::Unix(path) => Some(path),
            Self::Loopback { .. } => None,
        }
    }

    /// The port, when this is a loopback endpoint.
    pub(crate) fn port(&self) -> Option<u16> {
        match self {
            Self::Unix(_) => None,
            Self::Loopback { port, .. } => Some(*port),
        }
    }

    /// The credential, when this is a loopback endpoint.
    pub(crate) fn credential(&self) -> Option<&Credential> {
        match self {
            Self::Unix(_) => None,
            Self::Loopback { credential, .. } => Some(credential),
        }
    }
}

/// What a loopback listener demands of every connection.
///
/// A unix-socket listener in its own network namespace is isolated by the
/// namespace: only the sandbox can reach it. A loopback listener on a shared host
/// stack has no such premise, so without a credential it is an open decision
/// point — anything on the host that can open `127.0.0.1:<port>` gets to use the
/// rules, and the audit record then names a decision the sandbox never asked for.
///
/// Two random hex strings rather than one token: the same userinfo has to be
/// valid as `http://user:pass@host` and as `socks5h://user:pass@host`, and every
/// client library already reads credentials from the URL. The password is as
/// random as the user name because RFC 1929 asks for both.
#[derive(Clone, PartialEq, Eq)]
pub(crate) struct Credential {
    user: String,
    pass: String,
}

impl std::fmt::Debug for Credential {
    /// Never print the secret: this type is reachable from `Endpoint`, and
    /// `Endpoint` reaches logs and `Debug` output.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.Credential(<redacted>)")
    }
}

impl Credential {
    /// A fresh credential from the OS random source.
    pub(crate) fn generate() -> Self {
        Self {
            user: random_hex(16),
            pass: random_hex(16),
        }
    }

    /// `user:pass`, for a URL authority.
    pub(crate) fn userinfo(&self) -> String {
        format!("{}:{}", self.user, self.pass)
    }

    /// Whether an HTTP `Proxy-Authorization` value is this credential.
    ///
    /// Only `Basic` is accepted: it is what a client library already sends when
    /// the proxy URL carries userinfo, and it needs no configuration from the
    /// deployment.
    pub(crate) fn allows_basic(&self, presented: &str) -> bool {
        let Some(encoded) = presented.trim().strip_prefix("Basic ") else {
            return false;
        };
        use base64::Engine as _;
        let Some(decoded_bytes) = base64::engine::general_purpose::STANDARD
            .decode(encoded.trim())
            .ok()
        else {
            return false;
        };
        let Ok(decoded) = String::from_utf8(decoded_bytes) else {
            return false;
        };
        constant_time_eq(decoded.as_bytes(), self.userinfo().as_bytes())
    }

    /// Whether a SOCKS5 username/password pair is this credential (RFC 1929).
    pub(crate) fn allows_socks5(&self, user: &[u8], pass: &[u8]) -> bool {
        constant_time_eq(user, self.user.as_bytes()) && constant_time_eq(pass, self.pass.as_bytes())
    }
}

/// Compare without an early return, so a wrong guess does not reveal how much of
/// it was right. The lengths are not secret — they are fixed here.
fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    if left.len() != right.len() {
        return false;
    }
    let mut difference = 0u8;
    for (a, b) in left.iter().zip(right.iter()) {
        difference |= a ^ b;
    }
    difference == 0
}

/// `bytes` random bytes, hex-encoded.
fn random_hex(bytes: usize) -> String {
    use std::fmt::Write as _;
    let mut out = String::with_capacity(bytes * 2);
    for _ in 0..bytes {
        let _ = write!(out, "{:02x}", rand::random::<u8>());
    }
    out
}

/// One audit record: which destination was decided, and by what.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Decision {
    pub(crate) host: String,
    pub(crate) port: u16,
    pub(crate) allow: bool,
    pub(crate) mode: NetMode,
    pub(crate) rule: Option<String>,
    pub(crate) reason: Option<String>,
}

/// The proxy environment for a client that can reach `port` on loopback.
///
/// `NO_PROXY` is set to the empty string rather than removed: a deployment that
/// leaves `*` behind would send every connection straight past the only decision
/// point, and an explicit empty value is what overrides an inherited one.
pub(crate) fn environment(port: u16, credential: &Credential) -> Vec<(String, String)> {
    // The credential travels in the userinfo, which is where every client library
    // looks for it: `curl`, `git` and `pip` need no extra configuration, and that
    // is the whole reason Basic auth was chosen over a custom header.
    let userinfo = credential.userinfo();
    let http = format!("http://{userinfo}@127.0.0.1:{port}");
    let socks = format!("socks5h://{userinfo}@127.0.0.1:{port}");
    let mut env = Vec::with_capacity(OWNED_ENV.len());
    for (name, value) in [
        ("HTTP_PROXY", &http),
        ("http_proxy", &http),
        ("HTTPS_PROXY", &http),
        ("https_proxy", &http),
        ("ALL_PROXY", &socks),
        ("all_proxy", &socks),
    ] {
        env.push((name.to_owned(), value.clone()));
    }
    env.push(("NO_PROXY".to_owned(), String::new()));
    env.push(("no_proxy".to_owned(), String::new()));
    env
}

/// Receives one record per decision. Must not block.
pub(crate) type Audit = Arc<dyn Fn(&Decision) + Send + Sync>;

/// What to enforce.
pub(crate) struct Config {
    pub(crate) policy: NetPolicy,
    pub(crate) listen: Listen,
    pub(crate) audit: Option<Audit>,
    /// The bounds every live tunnel is held to; see [`ProxyLimits`].
    pub(crate) limits: ProxyLimits,
}

impl std::fmt::Debug for Config {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.ProxyConfig")
            .field("mode", &self.policy.mode)
            .field("listen", &self.listen)
            .field("limits", &self.limits)
            .finish_non_exhaustive()
    }
}

/// A running proxy.
pub(crate) struct Proxy {
    endpoint: Endpoint,
    handle: JoinHandle<()>,
}

impl std::fmt::Debug for Proxy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Proxy")
            .field("endpoint", &self.endpoint)
            .finish_non_exhaustive()
    }
}

impl Proxy {
    /// Where to reach it.
    pub(crate) fn endpoint(&self) -> &Endpoint {
        &self.endpoint
    }

    /// Stop accepting and drop the listener (and, for a unix endpoint, the file).
    pub(crate) async fn shutdown(self) {
        self.handle.abort();
        let _ = self.handle.await;
        if let Endpoint::Unix(path) = &self.endpoint {
            let _ = std::fs::remove_file(path);
        }
    }
}

/// Start the proxy.
pub(crate) async fn start(config: Config) -> Result<Proxy> {
    if config.policy.proxy.is_some() {
        // Legal in the policy (Go accepts it in both modes) but not implemented
        // here: refuse rather than silently dialling direct.
        return Err(Error::not_available(
            "sandbox: upstream proxy chaining is not implemented; drop `proxy` or use rules",
        ));
    }
    let matcher = Matcher::new(&config.policy)?;
    let mode = config.policy.mode;
    let audit = config.audit;
    let limits = config.limits;
    // A loopback listener is the one shape whose isolation does not come from a
    // network namespace, so it demands a credential; a unix socket is isolated by
    // the namespace itself and needs none.
    let loopback_credential = matches!(config.listen, Listen::Loopback).then(Credential::generate);
    let shared = Arc::new(Shared {
        matcher,
        mode,
        audit,
        // Exactly the configured slots. Zero refuses every connection, which is
        // the fail-closed reading of a limit that settings validation rejects; it
        // is never "unbounded".
        permits: Arc::new(Semaphore::new(limits.max_connections)),
        limits,
        credential: loopback_credential.clone(),
    });

    match config.listen {
        Listen::Loopback => {
            let listener = TcpListener::bind(("127.0.0.1", 0)).await.map_err(|error| {
                Error::not_available(format!("sandbox: bind the enforcement proxy: {error}"))
            })?;
            let port = listener
                .local_addr()
                .map_err(|error| Error::internal(format!("sandbox: proxy address: {error}")))?
                .port();
            let handle = tokio::spawn(async move {
                while let Ok((stream, _)) = listener.accept().await {
                    let shared = shared.clone();
                    tokio::spawn(async move {
                        let _ = serve_tcp(stream, shared).await;
                    });
                }
            });
            // The narrow credential above is built for exactly this arm, so a
            // missing one is an internal inconsistency, not a user error.
            let credential = loopback_credential.clone().ok_or_else(|| {
                Error::internal("sandbox: a loopback listener was started without a credential")
            })?;
            Ok(Proxy {
                endpoint: Endpoint::Loopback { port, credential },
                handle,
            })
        }
        Listen::Unix(path) => {
            #[cfg(not(unix))]
            {
                let _ = (&path, &shared);
                Err(Error::not_available(
                    "sandbox: a unix socket proxy listener is a unix mechanism",
                ))
            }
            #[cfg(unix)]
            {
                // A stale socket from a crashed run must not block the next one.
                let _ = std::fs::remove_file(&path);
                let listener = UnixListener::bind(&path).map_err(|error| {
                    Error::not_available(format!(
                        "sandbox: bind the enforcement proxy at {}: {error}",
                        path.display()
                    ))
                })?;
                let handle = tokio::spawn(async move {
                    while let Ok((stream, _)) = listener.accept().await {
                        let shared = shared.clone();
                        tokio::spawn(async move {
                            let _ = serve_unix(stream, shared).await;
                        });
                    }
                });
                Ok(Proxy {
                    endpoint: Endpoint::Unix(path),
                    handle,
                })
            }
        }
    }
}

/// What every connection shares.
struct Shared {
    matcher: Matcher,
    mode: NetMode,
    audit: Option<Audit>,
    /// `None` on a unix listener: the network namespace is the boundary there.
    credential: Option<Credential>,
    /// One permit per live forwarded connection. Refusing to hand one out is what
    /// keeps a burst of connections from growing the task set without bound.
    permits: Arc<Semaphore>,
    limits: ProxyLimits,
}

impl Shared {
    /// Take a slot for one live tunnel, or report that none is free.
    ///
    /// `try_acquire` rather than `acquire`: a connection over the cap is refused
    /// now, because waiting would turn a latency problem into a memory problem.
    fn reserve(&self) -> Option<OwnedSemaphorePermit> {
        self.permits.clone().try_acquire_owned().ok()
    }

    /// Decide one destination and record it.
    fn decide(&self, host: &str, port: u16) -> super::matcher::Verdict {
        // Resolve only when a rule needs an address; a hostname-only policy must
        // not pay for DNS on every request.
        let addrs = if self.matcher.has_ip_rules() || host.parse::<IpAddr>().is_ok() {
            resolve(host, port)
        } else {
            None
        };
        let verdict = self.matcher.decide(host, port, addrs.as_deref());
        if let Some(audit) = &self.audit {
            audit(&Decision {
                host: host.to_owned(),
                port,
                allow: verdict.allow,
                mode: self.mode,
                rule: verdict.rule.clone(),
                reason: verdict.reason.clone(),
            });
        }
        verdict
    }
}

/// Resolve a name, or report that it could not be done.
fn resolve(host: &str, port: u16) -> Option<Vec<IpAddr>> {
    if let Ok(ip) = host.parse::<IpAddr>() {
        return Some(vec![ip]);
    }
    let addrs = std::net::ToSocketAddrs::to_socket_addrs(&(host, port)).ok()?;
    let ips: Vec<IpAddr> = addrs.map(|addr| addr.ip()).collect();
    if ips.is_empty() { None } else { Some(ips) }
}

/// Dispatch one connection by its first byte.
///
/// The byte is read and then put back into the buffer the protocol parser gets,
/// so no protocol has to know that the dispatcher looked at it.
async fn serve_tcp(mut stream: TcpStream, shared: Arc<Shared>) -> Result<()> {
    let mut first = [0u8; 1];
    stream.read_exact(&mut first).await.map_err(Error::from)?;
    dispatch(stream, first.to_vec(), shared).await
}

#[cfg(unix)]
async fn serve_unix(mut stream: UnixStream, shared: Arc<Shared>) -> Result<()> {
    let mut first = [0u8; 1];
    stream.read_exact(&mut first).await.map_err(Error::from)?;
    dispatch(stream, first.to_vec(), shared).await
}

async fn dispatch<S>(stream: S, buffered: Vec<u8>, shared: Arc<Shared>) -> Result<()>
where
    S: AsyncRead + AsyncWrite + Unpin + Send,
{
    if buffered.first() == Some(&0x05) {
        return socks5(stream, buffered, shared).await;
    }
    http(stream, buffered, shared).await
}

/// Read one head, keeping the bytes already consumed.
///
/// Read in chunks rather than a byte at a time: the old loop spent one syscall
/// per byte, and the length of this head is whatever the client chooses to send.
/// The terminator test compares the last four bytes (O(1), independent of how
/// much has accumulated), so a terminator split across two reads is still found.
async fn read_head<S>(stream: &mut S, buffered: Vec<u8>) -> Result<Vec<u8>>
where
    S: AsyncRead + Unpin,
{
    let mut head = buffered;
    let mut chunk = [0u8; 1024];
    loop {
        if head.ends_with(b"\r\n\r\n") || head.ends_with(b"\n\n") {
            return Ok(head);
        }
        if head.len() >= MAX_HEAD {
            return Err(Error::validation(
                "sandbox: proxy request head is too large",
            ));
        }
        let want = (MAX_HEAD - head.len()).min(chunk.len());
        let read = stream.read(&mut chunk[..want]).await.map_err(Error::from)?;
        if read == 0 {
            return Err(Error::validation(
                "sandbox: proxy request head is truncated",
            ));
        }
        head.extend_from_slice(&chunk[..read]);
    }
}

/// Split a head into its request line and header lines.
fn parse_head(head: &[u8]) -> Option<(&str, Vec<&str>)> {
    let text = std::str::from_utf8(head).ok()?;
    let mut lines = text.split("\r\n").filter(|line| !line.is_empty());
    let request = lines.next()?;
    Some((request, lines.collect()))
}

fn header<'a>(headers: &[&'a str], name: &str) -> Option<&'a str> {
    headers.iter().find_map(|line| {
        let (key, value) = line.split_once(':')?;
        key.trim().eq_ignore_ascii_case(name).then(|| value.trim())
    })
}

async fn http<S>(mut stream: S, buffered: Vec<u8>, shared: Arc<Shared>) -> Result<()>
where
    S: AsyncRead + AsyncWrite + Unpin + Send,
{
    let head = read_head(&mut stream, buffered).await?;
    let (request, headers) = parse_head(&head)
        .ok_or_else(|| Error::validation("sandbox: proxy request has no request line"))?;
    // Authentication before authorization: a caller that has not proven it may
    // use this listener must not be able to probe the policy through it either.
    if let Some(credential) = &shared.credential
        && !credential.allows_basic(header(&headers, "proxy-authorization").unwrap_or_default())
    {
        return fail(&mut stream, 407, "proxy authentication required").await;
    }
    let mut parts = request.split_whitespace();
    let method = parts.next().unwrap_or_default().to_owned();
    let target = parts.next().unwrap_or_default().to_owned();

    if method.eq_ignore_ascii_case("CONNECT") {
        let (host, port) = split_authority(&target)?;
        let Some(endpoint) = authorize(&shared, &host, port) else {
            return deny(&mut stream, &shared, &host, port).await;
        };
        let Some(_permit) = shared.reserve() else {
            return fail(
                &mut stream,
                503,
                "the proxy is at its connection limit; retry shortly",
            )
            .await;
        };
        let tunnel = connect(endpoint).await;
        let Ok(tunnel) = tunnel else {
            return fail(
                &mut stream,
                502,
                "the proxy could not reach the destination",
            )
            .await;
        };
        stream
            .write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            .await
            .map_err(Error::from)?;
        return splice(stream, tunnel, &shared.limits).await;
    }

    // Plain HTTP: absolute-form carries the target, origin-form needs `Host`.
    let (host, port, path) = match target.find("://") {
        Some(at) => {
            let rest = &target[at + 3..];
            match rest.find('/') {
                Some(slash) => {
                    let authority = &rest[..slash];
                    let (host, port) = split_authority(authority)?;
                    (host, port, rest[slash..].to_owned())
                }
                None => {
                    let (host, port) = split_authority(rest)?;
                    (host, port, "/".to_owned())
                }
            }
        }
        None => {
            let authority = header(&headers, "host").unwrap_or_default().to_owned();
            let (host, port) = split_authority(&authority)?;
            (host, port, target.clone())
        }
    };
    let Some(endpoint) = authorize(&shared, &host, port) else {
        return deny(&mut stream, &shared, &host, port).await;
    };
    let Some(_permit) = shared.reserve() else {
        return fail(
            &mut stream,
            503,
            "the proxy is at its connection limit; retry shortly",
        )
        .await;
    };
    let Ok(upstream) = connect(endpoint).await else {
        return fail(
            &mut stream,
            502,
            "the proxy could not reach the destination",
        )
        .await;
    };

    // Rebuild the head in origin form, dropping the proxy's own headers and
    // replacing `Host` with the authority the rules actually validated: a
    // virtual-host front end would otherwise route the request to a destination
    // the decision never saw.
    let mut rewritten = format!("{method} {path} HTTP/1.1\r\n");
    for line in &headers {
        let name = line
            .split_once(':')
            .map(|(key, _)| key.trim())
            .unwrap_or_default();
        if name.eq_ignore_ascii_case("proxy-connection")
            || name.eq_ignore_ascii_case("proxy-authorization")
            || name.eq_ignore_ascii_case("host")
        {
            continue;
        }
        rewritten.push_str(line);
        rewritten.push_str("\r\n");
    }
    let authority = if port == 80 {
        host.clone()
    } else {
        format!("{host}:{port}")
    };
    rewritten.push_str(&format!("Host: {authority}\r\n\r\n"));
    let mut upstream = upstream;
    upstream
        .write_all(rewritten.as_bytes())
        .await
        .map_err(Error::from)?;
    splice(stream, upstream, &shared.limits).await
}

/// The dial target a decision authorises.
struct Authorized {
    host: String,
    port: u16,
    ip: Option<IpAddr>,
}

fn authorize(shared: &Shared, host: &str, port: u16) -> Option<Authorized> {
    let verdict = shared.decide(host, port);
    if !verdict.allow {
        return None;
    }
    Some(Authorized {
        host: host.to_owned(),
        port,
        ip: verdict.pin,
    })
}

/// Dial an authorised target, pinning the address when the decision did.
async fn connect(target: Authorized) -> Result<TcpStream> {
    match target.ip {
        Some(ip) => TcpStream::connect(SocketAddr::new(ip, target.port))
            .await
            .map_err(|error| Error::not_available(format!("sandbox: dial {ip}: {error}"))),
        None => TcpStream::connect((target.host.as_str(), target.port))
            .await
            .map_err(|error| {
                Error::not_available(format!(
                    "sandbox: dial {}:{}: {error}",
                    target.host, target.port
                ))
            }),
    }
}

async fn deny<S>(stream: &mut S, shared: &Shared, host: &str, port: u16) -> Result<()>
where
    S: AsyncWrite + Unpin,
{
    // The reason is model-facing on purpose: an agent that reads it learns why,
    // instead of retrying the same blocked call.
    let verdict = shared.matcher.decide(host, port, None);
    let reason = verdict
        .reason
        .unwrap_or_else(|| format!("{host}:{port} is not reachable under this policy"));
    fail(stream, 403, &reason).await
}

async fn fail<S>(stream: &mut S, code: u16, message: &str) -> Result<()>
where
    S: AsyncWrite + Unpin,
{
    let body = format!("{message}\n");
    let status = match code {
        403 => "Forbidden",
        407 => "Proxy Authentication Required",
        503 => "Service Unavailable",
        _ => "Bad Gateway",
    };
    let head = format!(
        "HTTP/1.1 {code} {status}\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    stream
        .write_all(head.as_bytes())
        .await
        .map_err(Error::from)?;
    stream.shutdown().await.map_err(Error::from)
}

/// Copy both directions until the tunnel ends or outlives one of its bounds.
///
/// `copy_bidirectional` only knows when the copies finish, not whether they are
/// making progress, so the idle and lifetime bounds are enforced by watching
/// [`Observed`]: a tunnel that moves no bytes for `idle_timeout`, or stays open
/// for `max_lifetime` however busy it is, has both directions closed.
async fn splice<A, B>(client: A, upstream: B, limits: &ProxyLimits) -> Result<()>
where
    A: AsyncRead + AsyncWrite + Unpin + Send,
    B: AsyncRead + AsyncWrite + Unpin + Send,
{
    // `copy_bidirectional` has no idle notion: it bounds how much is copied, not
    // the gap between bytes, so `timeout(max_lifetime, copy_bidirectional(..))`
    // would kill a long-poll tunnel that is idle by design but alive. The
    // wrapper records when a byte last moved — in either direction, since every
    // byte is a completed read on one side — and the two bounds are *raced*
    // against the copy rather than wrapped around it.
    let activity = Arc::new(Activity::new());
    let mut client = Observed {
        inner: client,
        activity: Arc::clone(&activity),
    };
    let mut upstream = Observed {
        inner: upstream,
        activity: Arc::clone(&activity),
    };
    let copy = tokio::io::copy_bidirectional(&mut client, &mut upstream);
    tokio::select! {
        copied = copy => {
            // A reset is the client's own business; anything else is worth a
            // line, because it ended a tunnel the caller expected to work.
            if let Err(error) = copied
                && error.kind() != std::io::ErrorKind::ConnectionReset
            {
                tracing::debug!(%error, "sandbox: proxy tunnel ended early");
            }
            Ok(())
        }
        bound = bound_exceeded(Arc::clone(&activity), limits) => {
            // Returning drops both halves, which closes both directions: leaving
            // one half-closed would keep a descriptor alive past the tunnel.
            match bound {
                Bound::Idle => tracing::debug!(
                    idle_secs = limits.idle_timeout.as_secs(),
                    "sandbox: proxy tunnel closed after an idle period"
                ),
                Bound::Lifetime => tracing::debug!(
                    lifetime_secs = limits.max_lifetime.as_secs(),
                    "sandbox: proxy tunnel closed at its lifetime cap"
                ),
            }
            Ok(())
        }
    }
}

/// Why a splice stopped, where it stopped itself rather than the client.
#[derive(Debug, Clone, Copy)]
enum Bound {
    Idle,
    Lifetime,
}

/// What the two time bounds count: when the tunnel opened, and when a byte last
/// moved through it.
///
/// The offsets are milliseconds from `opened` because an `Instant` does not fit
/// an atomic, and the clock is the tokio one so a paused test clock measures the
/// same time the watchdog waits on.
struct Activity {
    opened: Instant,
    last_millis: AtomicU64,
}

impl Activity {
    fn new() -> Self {
        Self {
            opened: Instant::now(),
            last_millis: AtomicU64::new(0),
        }
    }

    /// A byte moved: every byte in either direction is a completed read on one
    /// side, so the read path is the only place that has to touch this.
    fn touch(&self) {
        let millis = self.opened.elapsed().as_millis().min(u128::from(u64::MAX)) as u64;
        self.last_millis.store(millis, Ordering::Relaxed);
    }

    fn last(&self) -> Instant {
        self.opened + Duration::from_millis(self.last_millis.load(Ordering::Relaxed))
    }
}

/// Resolves when the tunnel has been quiet for `idle_timeout` or open for
/// `max_lifetime`, whichever comes first.
///
/// The deadline is recomputed on every wake-up, so a byte that lands while the
/// watchdog sleeps refreshes the idle deadline without restarting a task. A
/// disabled bound (`0`) is left out of the comparison; with both disabled this
/// never resolves, because only the client ending the tunnel can.
async fn bound_exceeded(activity: Arc<Activity>, limits: &ProxyLimits) -> Bound {
    loop {
        let now = Instant::now();
        let idle_at =
            (!limits.idle_timeout.is_zero()).then(|| activity.last() + limits.idle_timeout);
        let lifetime_at =
            (!limits.max_lifetime.is_zero()).then(|| activity.opened + limits.max_lifetime);
        let next = match (idle_at, lifetime_at) {
            (Some(idle), Some(lifetime)) => {
                if lifetime < idle {
                    (lifetime, Bound::Lifetime)
                } else {
                    (idle, Bound::Idle)
                }
            }
            (Some(idle), None) => (idle, Bound::Idle),
            (None, Some(lifetime)) => (lifetime, Bound::Lifetime),
            (None, None) => return std::future::pending().await,
        };
        if next.0 <= now {
            return next.1;
        }
        tokio::time::sleep_until(next.0).await;
    }
}

/// A stream that records when a byte last moved through it.
///
/// Wrapping both ends is what gives `copy_bidirectional` an idle notion: the
/// copies keep their existing half-close behaviour, and the wrapper is the only
/// place that knows they are still making progress.
struct Observed<S> {
    inner: S,
    activity: Arc<Activity>,
}

impl<S: AsyncRead + Unpin> AsyncRead for Observed<S> {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let this = self.get_mut();
        let before = buf.filled().len();
        let poll = Pin::new(&mut this.inner).poll_read(cx, buf);
        if let Poll::Ready(Ok(())) = &poll
            && buf.filled().len() > before
        {
            this.activity.touch();
        }
        poll
    }
}

impl<S: AsyncWrite + Unpin> AsyncWrite for Observed<S> {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.get_mut().inner).poll_write(cx, buf)
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_flush(cx)
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_shutdown(cx)
    }
}

/// `host:port`, `host`, `[::1]:80`.
fn split_authority(raw: &str) -> Result<(String, u16)> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Err(Error::validation(
            "sandbox: proxy request has no destination",
        ));
    }
    if let Some(rest) = raw.strip_prefix('[') {
        let (host, tail) = rest
            .split_once(']')
            .ok_or_else(|| Error::validation(format!("sandbox: bad authority {raw:?}")))?;
        let port = match tail.strip_prefix(':') {
            Some(port) => port
                .parse()
                .map_err(|_| Error::validation(format!("sandbox: bad port in {raw:?}")))?,
            None => 443,
        };
        return Ok((host.to_owned(), port));
    }
    match raw.rsplit_once(':') {
        Some((host, port)) if !host.is_empty() => Ok((
            host.to_owned(),
            port.parse()
                .map_err(|_| Error::validation(format!("sandbox: bad port in {raw:?}")))?,
        )),
        // No port: the scheme default is 443 for CONNECT and 80 for plain HTTP;
        // the caller of `CONNECT` always sends one, so this is the HTTP case.
        _ => Ok((raw.to_owned(), 80)),
    }
}

/// The SOCKS5 half: no authentication, `CONNECT` only.
async fn socks5<S>(mut stream: S, buffered: Vec<u8>, shared: Arc<Shared>) -> Result<()>
where
    S: AsyncRead + AsyncWrite + Unpin + Send,
{
    // Greeting: VER, NMETHODS, METHODS...
    let mut greeting = buffered;
    let mut byte = [0u8; 1];
    while greeting.len() < 2 {
        stream.read_exact(&mut byte).await.map_err(Error::from)?;
        greeting.push(byte[0]);
    }
    let methods = usize::from(greeting[1]);
    while greeting.len() < 2 + methods {
        stream.read_exact(&mut byte).await.map_err(Error::from)?;
        greeting.push(byte[0]);
    }
    // With a credential the only acceptable method is username/password
    // (RFC 1929, `0x02`); without one it is "no authentication" (`0x00`). A
    // listener that offered both would let a client pick the weaker one.
    let wanted = if shared.credential.is_some() {
        0x02
    } else {
        0x00
    };
    if !greeting[2..2 + methods].contains(&wanted) {
        stream.write_all(&[0x05, 0xff]).await.map_err(Error::from)?;
        return Ok(());
    }
    stream
        .write_all(&[0x05, wanted])
        .await
        .map_err(Error::from)?;
    if let Some(credential) = &shared.credential {
        // Sub-negotiation: VER=0x01, ULEN, UNAME, PLEN, PASSWD.
        let mut version = [0u8; 1];
        stream.read_exact(&mut version).await.map_err(Error::from)?;
        let mut user_len = [0u8; 1];
        stream
            .read_exact(&mut user_len)
            .await
            .map_err(Error::from)?;
        let mut user = vec![0u8; usize::from(user_len[0])];
        stream.read_exact(&mut user).await.map_err(Error::from)?;
        let mut pass_len = [0u8; 1];
        stream
            .read_exact(&mut pass_len)
            .await
            .map_err(Error::from)?;
        let mut pass = vec![0u8; usize::from(pass_len[0])];
        stream.read_exact(&mut pass).await.map_err(Error::from)?;
        // A wrong version is a protocol error, and a wrong pair is a rejected
        // login; both end the connection with a status the client can report.
        let ok = version[0] == 0x01 && credential.allows_socks5(&user, &pass);
        stream
            .write_all(&[0x01, if ok { 0x00 } else { 0x01 }])
            .await
            .map_err(Error::from)?;
        if !ok {
            return Ok(());
        }
    }

    // Request: VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT
    let mut request = [0u8; 4];
    stream.read_exact(&mut request).await.map_err(Error::from)?;
    if request[1] != 0x01 {
        stream
            .write_all(&[0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await
            .map_err(Error::from)?;
        return Ok(());
    }
    let host = match request[3] {
        0x01 => {
            let mut addr = [0u8; 4];
            stream.read_exact(&mut addr).await.map_err(Error::from)?;
            IpAddr::from(addr).to_string()
        }
        0x04 => {
            let mut addr = [0u8; 16];
            stream.read_exact(&mut addr).await.map_err(Error::from)?;
            IpAddr::from(addr).to_string()
        }
        0x03 => {
            let mut length = [0u8; 1];
            stream.read_exact(&mut length).await.map_err(Error::from)?;
            let mut name = vec![0u8; usize::from(length[0])];
            stream.read_exact(&mut name).await.map_err(Error::from)?;
            String::from_utf8_lossy(&name).into_owned()
        }
        _ => {
            return Err(Error::validation(
                "sandbox: unsupported SOCKS5 address type",
            ));
        }
    };
    let mut port = [0u8; 2];
    stream.read_exact(&mut port).await.map_err(Error::from)?;
    let port = u16::from_be_bytes(port);

    let Some(target) = authorize(&shared, &host, port) else {
        // 0x02 = connection not allowed by ruleset.
        stream
            .write_all(&[0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await
            .map_err(Error::from)?;
        return Ok(());
    };
    let Some(_permit) = shared.reserve() else {
        // 0x01 = general SOCKS server failure: the protocol has no "busy" code,
        // and 0x02 would misreport a policy denial as the reason.
        stream
            .write_all(&[0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await
            .map_err(Error::from)?;
        return Ok(());
    };
    let Ok(upstream) = connect(target).await else {
        stream
            .write_all(&[0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await
            .map_err(Error::from)?;
        return Ok(());
    };
    stream
        .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
        .await
        .map_err(Error::from)?;
    splice(stream, upstream, &shared.limits).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::{NetAction, NetRule};

    fn authority(raw: &str) -> (String, u16) {
        split_authority(raw).expect("authority")
    }

    #[test]
    fn an_authority_is_split_into_host_and_port() {
        assert_eq!(
            authority("example.com:443"),
            ("example.com".to_owned(), 443)
        );
        assert_eq!(authority("[::1]:8443"), ("::1".to_owned(), 8443));
        assert_eq!(authority("example.com"), ("example.com".to_owned(), 80));
        assert!(split_authority("").is_err());
        assert!(split_authority("[::1").is_err());
        assert!(split_authority("example.com:notaport").is_err());
    }

    #[test]
    fn a_head_is_split_into_its_request_line_and_headers() {
        let head = b"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com\r\nProxy-Connection: keep-alive\r\n\r\n";
        let (request, headers) = parse_head(head).expect("head");
        assert_eq!(request, "CONNECT example.com:443 HTTP/1.1");
        assert_eq!(header(&headers, "host"), Some("example.com"));
        assert_eq!(header(&headers, "HOST"), Some("example.com"));
        assert_eq!(header(&headers, "missing"), None);
    }

    /// The head is read in chunks now, so the two boundaries that matter are a
    /// terminator split across the dispatcher's byte and the first read, and one
    /// split across two reads.
    #[tokio::test]
    async fn a_head_split_across_reads_is_still_found() {
        // `\r\n\r` arrives with the byte the dispatcher consumed, `\n` with the
        // next read: a 4-byte terminator straddling the boundary.
        let buffered = b"CONNECT example.com:443 HTTP/1.1\r\n\r".to_vec();
        let mut reader: &[u8] = b"\n";
        let head = read_head(&mut reader, buffered).await.expect("head");
        assert!(
            head.ends_with(b"\r\n\r\n"),
            "{:?}",
            String::from_utf8_lossy(&head)
        );

        // The bare-LF terminator takes the other branch.
        let buffered = b"GET / HTTP/1.1\n".to_vec();
        let mut reader: &[u8] = b"\n";
        assert!(read_head(&mut reader, buffered).await.is_ok());
    }

    #[tokio::test]
    async fn an_unterminated_head_is_bounded_and_reported() {
        // Truncated: the peer closed before the head ended.
        let mut reader: &[u8] = b"";
        let error = read_head(&mut reader, b"GET / HTTP/1.1\r\n".to_vec())
            .await
            .expect_err("truncated");
        assert!(error.to_string().contains("truncated"), "{error}");

        // Unbounded: the peer keeps sending. The loop must stop at the cap
        // instead of growing, which is also what keeps a client from choosing
        // the allocation size.
        let flood = vec![b'A'; MAX_HEAD + 64];
        let mut reader: &[u8] = &flood;
        let error = read_head(&mut reader, b"G".to_vec())
            .await
            .expect_err("too large");
        assert!(error.to_string().contains("too large"), "{error}");
    }

    #[test]
    fn the_owned_proxy_variables_are_exactly_the_ones_we_set() {
        // The list is what a caller may not override; adding a variable to one
        // side without the other would be a bypass.
        for name in OWNED_ENV {
            assert!(
                name.eq_ignore_ascii_case("http_proxy")
                    || name.eq_ignore_ascii_case("https_proxy")
                    || name.eq_ignore_ascii_case("all_proxy")
                    || name.eq_ignore_ascii_case("no_proxy")
            );
        }
        assert_eq!(OWNED_ENV.len(), 8, "upper and lower case for four names");
    }

    #[test]
    fn an_upstream_chain_is_refused_rather_than_ignored() {
        let policy = NetPolicy {
            mode: NetMode::Proxy,
            proxy: Some("http://127.0.0.1:3128".to_owned()),
            ..NetPolicy::default()
        };
        let error = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(start(Config {
                policy,
                listen: Listen::Loopback,
                audit: None,
                limits: ProxyLimits::default(),
            }))
            .expect_err("not implemented");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        assert!(error.to_string().contains("not implemented"), "{error}");
    }

    fn policy(mode: NetMode, rules: Vec<NetRule>) -> NetPolicy {
        NetPolicy {
            mode,
            rules,
            ..NetPolicy::default()
        }
    }

    /// A loopback server that echoes everything it reads, so a tunnel is
    /// observable for as long as the client keeps it busy.
    async fn echo() -> (u16, JoinHandle<()>) {
        let listener = TcpListener::bind(("127.0.0.1", 0)).await.expect("bind");
        let port = listener.local_addr().expect("addr").port();
        let handle = tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                tokio::spawn(async move {
                    let mut buffer = [0u8; 64];
                    while let Ok(read) = stream.read(&mut buffer).await {
                        if read == 0 || stream.write_all(&buffer[..read]).await.is_err() {
                            break;
                        }
                    }
                });
            }
        });
        (port, handle)
    }

    fn allow_loopback(port: u16) -> NetPolicy {
        policy(
            NetMode::AllowList,
            vec![NetRule {
                action: NetAction::Allow,
                host: "127.0.0.1".to_owned(),
                port,
                reason: None,
            }],
        )
    }

    async fn proxy_with(policy: NetPolicy, limits: ProxyLimits) -> Proxy {
        start(Config {
            policy,
            listen: Listen::Loopback,
            audit: None,
            limits,
        })
        .await
        .expect("proxy")
    }

    /// The `Proxy-Authorization` value this proxy demands, if any.
    fn authorization(proxy: &Proxy) -> Option<String> {
        use base64::Engine as _;
        proxy.endpoint().credential().map(|credential| {
            format!(
                "Basic {}",
                base64::engine::general_purpose::STANDARD.encode(credential.userinfo())
            )
        })
    }

    /// A `CONNECT` head that authenticates the way a client with the proxy URL in
    /// its environment would.
    fn connect_head(proxy: &Proxy, port: u16) -> String {
        let mut head = format!("CONNECT 127.0.0.1:{port} HTTP/1.1\r\nHost: 127.0.0.1\r\n");
        if let Some(value) = authorization(proxy) {
            head.push_str(&format!("Proxy-Authorization: {value}\r\n"));
        }
        head.push_str("\r\n");
        head
    }

    /// Open a `CONNECT` tunnel, or report that the proxy refused it with a 503.
    async fn tunnel(proxy: &Proxy, port: u16) -> Option<TcpStream> {
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = connect_head(proxy, port);
        client.write_all(request.as_bytes()).await.expect("write");
        let mut head = vec![0u8; 39];
        client.read_exact(&mut head).await.expect("head");
        let head = String::from_utf8_lossy(&head).into_owned();
        if head.starts_with("HTTP/1.1 200") {
            Some(client)
        } else {
            assert!(head.starts_with("HTTP/1.1 503"), "{head}");
            None
        }
    }

    /// Complete the SOCKS5 greeting and login, leaving the stream post-auth.
    async fn socks5_authenticate(proxy: &Proxy) -> TcpStream {
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let credential = proxy
            .endpoint()
            .credential()
            .expect("a loopback credential");
        client
            .write_all(&[0x05, 0x01, 0x02])
            .await
            .expect("greeting");
        let mut method = [0u8; 2];
        client.read_exact(&mut method).await.expect("method");
        assert_eq!(method, [0x05, 0x02]);
        let userinfo = credential.userinfo();
        let (user, pass) = userinfo.split_once(':').expect("user:pass");
        let mut login = vec![0x01, user.len() as u8];
        login.extend_from_slice(user.as_bytes());
        login.push(pass.len() as u8);
        login.extend_from_slice(pass.as_bytes());
        client.write_all(&login).await.expect("login");
        let mut status = [0u8; 2];
        client.read_exact(&mut status).await.expect("status");
        assert_eq!(status, [0x01, 0x00], "the credential is accepted");
        client
    }

    /// Run a SOCKS5 `CONNECT` and return the client plus the reply code.
    async fn socks5_connect(proxy: &Proxy, port: u16) -> (TcpStream, u8) {
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let credential = proxy
            .endpoint()
            .credential()
            .expect("a loopback credential");
        client
            .write_all(&[0x05, 0x01, 0x02])
            .await
            .expect("greeting");
        let mut method = [0u8; 2];
        client.read_exact(&mut method).await.expect("method");
        assert_eq!(method, [0x05, 0x02]);
        // RFC 1929 sub-negotiation.
        let user = credential.userinfo();
        let (user, pass) = user.split_once(':').expect("user:pass");
        let mut login = vec![0x01, user.len() as u8];
        login.extend_from_slice(user.as_bytes());
        login.push(pass.len() as u8);
        login.extend_from_slice(pass.as_bytes());
        client.write_all(&login).await.expect("login");
        let mut status = [0u8; 2];
        client.read_exact(&mut status).await.expect("status");
        assert_eq!(status, [0x01, 0x00], "the credential is accepted");
        let mut request = vec![0x05, 0x01, 0x00, 0x01];
        request.extend_from_slice(&[127, 0, 0, 1]);
        request.extend_from_slice(&port.to_be_bytes());
        client.write_all(&request).await.expect("request");
        let mut reply = [0u8; 10];
        client.read_exact(&mut reply).await.expect("reply");
        (client, reply[1])
    }

    #[tokio::test]
    async fn a_connect_tunnel_reaches_an_allowed_destination() {
        let (port, _server) = echo().await;
        let proxy = start(Config {
            policy: policy(
                NetMode::AllowList,
                vec![NetRule {
                    action: NetAction::Allow,
                    host: "127.0.0.1".to_owned(),
                    port,
                    reason: None,
                }],
            ),
            listen: Listen::Loopback,
            audit: None,
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");

        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = connect_head(&proxy, port);
        client.write_all(request.as_bytes()).await.expect("write");
        let mut head = vec![0u8; 39];
        client.read_exact(&mut head).await.expect("head");
        assert!(
            String::from_utf8_lossy(&head).starts_with("HTTP/1.1 200"),
            "{}",
            String::from_utf8_lossy(&head)
        );
        // The tunnel carries bytes both ways.
        client.write_all(b"ping").await.expect("write");
        let mut echoed = [0u8; 4];
        client.read_exact(&mut echoed).await.expect("echo");
        assert_eq!(&echoed, b"ping");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_denied_destination_gets_the_reason_and_no_tunnel() {
        let (port, _server) = echo().await;
        let audit = Arc::new(std::sync::Mutex::new(Vec::<Decision>::new()));
        let recorded = audit.clone();
        let proxy = start(Config {
            policy: policy(
                NetMode::AllowList,
                vec![NetRule {
                    action: NetAction::Deny,
                    host: "127.0.0.1".to_owned(),
                    port,
                    reason: Some("loopback is not a destination".to_owned()),
                }],
            ),
            listen: Listen::Loopback,
            audit: Some(Arc::new(move |decision: &Decision| {
                recorded.lock().expect("lock").push(decision.clone());
            })),
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");

        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = connect_head(&proxy, port);
        client.write_all(request.as_bytes()).await.expect("write");
        let mut response = String::new();
        client.read_to_string(&mut response).await.expect("read");
        assert!(response.starts_with("HTTP/1.1 403"), "{response}");
        assert!(
            response.contains("loopback is not a destination"),
            "the reason must reach the caller: {response}"
        );
        let decisions = audit.lock().expect("lock").clone();
        assert_eq!(decisions.len(), 1);
        assert!(!decisions[0].allow);
        assert_eq!(decisions[0].mode, NetMode::AllowList);
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn the_forwarded_host_is_the_validated_one() {
        // A listener that reports the request head it received.
        let listener = TcpListener::bind(("127.0.0.1", 0)).await.expect("bind");
        let upstream_port = listener.local_addr().expect("addr").port();
        let seen = Arc::new(std::sync::Mutex::new(String::new()));
        let recorded = seen.clone();
        tokio::spawn(async move {
            if let Ok((mut stream, _)) = listener.accept().await {
                let mut buffer = [0u8; 1024];
                let read = stream.read(&mut buffer).await.unwrap_or(0);
                *recorded.lock().expect("lock") =
                    String::from_utf8_lossy(&buffer[..read]).into_owned();
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                    .await;
            }
        });

        let proxy = start(Config {
            policy: policy(NetMode::Proxy, vec![]),
            listen: Listen::Loopback,
            audit: None,
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        // Absolute-form to the validated authority, with a decoy `Host`.
        let auth = authorization(&proxy)
            .map(|value| format!("Proxy-Authorization: {value}\r\n"))
            .unwrap_or_default();
        let request = format!(
            "GET http://127.0.0.1:{upstream_port}/ HTTP/1.1\r\nHost: internal.corp\r\n{auth}\r\n"
        );
        client.write_all(request.as_bytes()).await.expect("write");
        let mut response = Vec::new();
        let _ = client.read(&mut response).await;
        let head = seen.lock().expect("lock").clone();
        assert!(
            head.contains(&format!("Host: 127.0.0.1:{upstream_port}")),
            "the upstream must see the authority the rules validated: {head:?}"
        );
        assert!(!head.contains("internal.corp"), "{head:?}");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_socks5_connect_goes_through_the_same_decision() {
        let (port, _server) = echo().await;
        let proxy = start(Config {
            policy: policy(
                NetMode::AllowList,
                vec![NetRule {
                    action: NetAction::Allow,
                    host: "127.0.0.1".to_owned(),
                    port,
                    reason: None,
                }],
            ),
            listen: Listen::Loopback,
            audit: None,
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");

        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        client
            .write_all(&[0x05, 0x01, 0x02])
            .await
            .expect("greeting");
        let mut method = [0u8; 2];
        client.read_exact(&mut method).await.expect("method");
        assert_eq!(method, [0x05, 0x02]);
        // Log in; the credential comes from the endpoint the sandbox was given.
        let credential = proxy.endpoint().credential().expect("credential");
        let userinfo = credential.userinfo();
        let (user, pass) = userinfo.split_once(':').expect("user:pass");
        let mut login = vec![0x01, user.len() as u8];
        login.extend_from_slice(user.as_bytes());
        login.push(pass.len() as u8);
        login.extend_from_slice(pass.as_bytes());
        client.write_all(&login).await.expect("login");
        let mut status = [0u8; 2];
        client.read_exact(&mut status).await.expect("status");
        assert_eq!(status, [0x01, 0x00]);

        let mut request = vec![0x05, 0x01, 0x00, 0x01];
        request.extend_from_slice(&[127, 0, 0, 1]);
        request.extend_from_slice(&port.to_be_bytes());
        client.write_all(&request).await.expect("request");
        let mut reply = [0u8; 10];
        client.read_exact(&mut reply).await.expect("reply");
        assert_eq!(reply[1], 0x00, "socks5 CONNECT succeeded");

        client.write_all(b"ping").await.expect("write");
        let mut echoed = [0u8; 4];
        client.read_exact(&mut echoed).await.expect("echo");
        assert_eq!(&echoed, b"ping");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_socks5_denial_is_reported_in_the_protocol() {
        let (port, _server) = echo().await;
        let proxy = start(Config {
            policy: policy(NetMode::AllowList, vec![]),
            listen: Listen::Loopback,
            audit: None,
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        // Denied *after* the credential is accepted: the policy verdict, not the
        // handshake, is what this test is about.
        let mut client = socks5_authenticate(&proxy).await;
        let mut request = vec![0x05, 0x01, 0x00, 0x01];
        request.extend_from_slice(&[127, 0, 0, 1]);
        request.extend_from_slice(&port.to_be_bytes());
        client.write_all(&request).await.expect("request");
        let mut reply = [0u8; 10];
        client.read_exact(&mut reply).await.expect("reply");
        assert_eq!(reply[1], 0x02, "not allowed by ruleset");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_connection_over_the_concurrency_cap_is_refused_at_once() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                max_connections: 2,
                ..ProxyLimits::default()
            },
        )
        .await;
        let proxy_port = proxy.endpoint().port().expect("port");

        // The first `max_connections` tunnels are accepted and carry bytes.
        let mut held = Vec::new();
        for _ in 0..2 {
            let mut client = tunnel(&proxy, port).await.expect("under the cap");
            client.write_all(b"ping").await.expect("write");
            let mut echoed = [0u8; 4];
            client.read_exact(&mut echoed).await.expect("echo");
            assert_eq!(&echoed, b"ping");
            held.push(client);
        }

        // The next one is refused now rather than parked. `tunnel` asserts the
        // 200 head, so this one is read whole to see the 503.
        let mut refused = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = connect_head(&proxy, port);
        refused.write_all(request.as_bytes()).await.expect("write");
        let mut response = String::new();
        refused.read_to_string(&mut response).await.expect("read");
        assert!(response.starts_with("HTTP/1.1 503"), "{response}");

        // A refusal is not a teardown: the tunnels already holding slots work.
        held[0].write_all(b"pong").await.expect("write");
        let mut echoed = [0u8; 4];
        held[0].read_exact(&mut echoed).await.expect("echo");
        assert_eq!(&echoed, b"pong");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_socks5_handshake_over_the_concurrency_cap_is_refused_in_the_protocol() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                max_connections: 1,
                ..ProxyLimits::default()
            },
        )
        .await;

        let (held, code) = socks5_connect(&proxy, port).await;
        assert_eq!(code, 0x00, "the first tunnel fits");
        let (_, code) = socks5_connect(&proxy, port).await;
        assert_eq!(
            code, 0x01,
            "a capacity refusal is a general SOCKS5 failure, not a policy denial"
        );
        drop(held);
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_refused_connection_does_not_keep_its_slot() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                max_connections: 1,
                ..ProxyLimits::default()
            },
        )
        .await;
        let proxy_port = proxy.endpoint().port().expect("port");

        // Fill the only slot, then refuse one that never held a permit.
        let held = tunnel(&proxy, port).await.expect("first tunnel");
        let mut refused = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = connect_head(&proxy, port);
        refused.write_all(request.as_bytes()).await.expect("write");
        let mut response = String::new();
        refused.read_to_string(&mut response).await.expect("read");
        assert!(response.starts_with("HTTP/1.1 503"), "{response}");
        drop((held, refused));

        // Closing the two connections releases the one permit; a refusal must
        // not have kept a slot of its own, or this never gets in.
        let mut accepted = None;
        for _ in 0..100 {
            if let Some(client) = tunnel(&proxy, port).await {
                accepted = Some(client);
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(
            accepted.is_some(),
            "a refused connection must not leak the permit it never held"
        );
        proxy.shutdown().await;
    }

    /// Real durations, not a paused clock: a paused tokio clock auto-advances
    /// whenever the runtime has nothing to poll, and a real `TcpStream` makes it
    /// idle between round trips — so a 60 s bound would fire after a few
    /// milliseconds of wall time. The bound is about ordering, not about how long
    /// the test waits, so 100 ms says the same thing and cannot flake.
    #[tokio::test]
    async fn an_idle_tunnel_is_closed_after_the_idle_timeout() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                idle_timeout: Duration::from_millis(100),
                ..ProxyLimits::default()
            },
        )
        .await;
        let mut client = tunnel(&proxy, port).await.expect("tunnel");

        // Fresh, the tunnel carries bytes.
        client.write_all(b"ping").await.expect("write");
        let mut echoed = [0u8; 4];
        client.read_exact(&mut echoed).await.expect("echo");
        assert_eq!(&echoed, b"ping");

        // Then it moves nothing, and the idle bound closes both directions.
        let mut byte = [0u8; 1];
        let read = tokio::time::timeout(Duration::from_secs(120), client.read(&mut byte)).await;
        match read {
            Ok(Ok(0)) | Ok(Err(_)) => {}
            other => panic!("the idle tunnel must be closed: {other:?}"),
        }
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_tunnel_that_keeps_sending_is_not_closed_by_the_idle_timeout() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                idle_timeout: Duration::from_millis(300),
                ..ProxyLimits::default()
            },
        )
        .await;
        let mut client = tunnel(&proxy, port).await.expect("tunnel");

        // A byte every 50 ms for 400 ms: longer than the 300 ms idle bound, in
        // gaps far shorter than it. A long-poll or SSE tunnel looks like this; a
        // `timeout(idle, copy_bidirectional)` would have killed it at 300 ms.
        for _ in 0..8 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            client.write_all(b"x").await.expect("write");
            let mut echoed = [0u8; 1];
            client
                .read_exact(&mut echoed)
                .await
                .expect("a tunnel that keeps sending must stay open");
        }
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_busy_tunnel_is_closed_at_the_lifetime_cap() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(
            allow_loopback(port),
            ProxyLimits {
                max_lifetime: Duration::from_millis(200),
                idle_timeout: Duration::from_secs(30),
                ..ProxyLimits::default()
            },
        )
        .await;
        let mut client = tunnel(&proxy, port).await.expect("tunnel");
        let opened = Instant::now();

        // Busy the whole time: never idle (30 s bound, 50 ms gaps), and still
        // ended by the lifetime cap, because "busy forever" is still "never ends".
        let mut closed = false;
        for _ in 0..40 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            if client.write_all(b"x").await.is_err() {
                closed = true;
                break;
            }
            let mut echoed = [0u8; 1];
            if client.read_exact(&mut echoed).await.is_err() {
                closed = true;
                break;
            }
        }
        assert!(
            closed,
            "a tunnel must not outlive max_lifetime just by staying busy"
        );
        assert!(
            opened.elapsed() >= Duration::from_millis(200),
            "the cap must not fire early: {:?}",
            opened.elapsed()
        );
        proxy.shutdown().await;
    }

    /// A loopback listener shares the host network stack, so it has no namespace
    /// to hide behind: without a credential, anything on the host that can reach
    /// the port gets to use the decision point (and to appear in its audit trail).
    #[tokio::test]
    async fn a_connect_without_the_credential_is_refused_before_the_decision() {
        let (port, _server) = echo().await;
        let audit = Arc::new(std::sync::Mutex::new(Vec::<Decision>::new()));
        let recorded = audit.clone();
        let proxy = start(Config {
            policy: policy(
                NetMode::AllowList,
                vec![NetRule {
                    action: NetAction::Allow,
                    host: "127.0.0.1".to_owned(),
                    port,
                    reason: None,
                }],
            ),
            listen: Listen::Loopback,
            audit: Some(Arc::new(move |decision: &Decision| {
                recorded.lock().expect("lock").push(decision.clone());
            })),
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");

        for header in [
            // No header at all.
            String::new(),
            // A plausible-looking one, which is the case a "does it start with
            // Basic" check would wave through.
            format!("Proxy-Authorization: Basic {}\r\n", {
                use base64::Engine as _;
                base64::engine::general_purpose::STANDARD.encode("wrong:wrong")
            }),
        ] {
            let proxy_port = proxy.endpoint().port().expect("port");
            let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
                .await
                .expect("connect");
            let request =
                format!("CONNECT 127.0.0.1:{port} HTTP/1.1\r\nHost: 127.0.0.1\r\n{header}\r\n");
            client.write_all(request.as_bytes()).await.expect("write");
            let mut response = String::new();
            client.read_to_string(&mut response).await.expect("read");
            assert!(
                response.starts_with("HTTP/1.1 407"),
                "an unauthenticated caller must be refused: {response}"
            );
        }
        // And nothing reached the policy: an unauthenticated caller cannot probe
        // which destinations are allowed through it.
        assert!(
            audit.lock().expect("lock").is_empty(),
            "no decision was made"
        );
        proxy.shutdown().await;
    }

    /// RFC 1929 asks for both fields, and a listener that offered "no
    /// authentication" beside it would let the client pick the weaker method.
    #[tokio::test]
    async fn a_socks5_greeting_without_the_password_method_is_refused() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(allow_loopback(port), ProxyLimits::default()).await;
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        client
            .write_all(&[0x05, 0x01, 0x00])
            .await
            .expect("greeting");
        let mut method = [0u8; 2];
        client.read_exact(&mut method).await.expect("method");
        assert_eq!(method, [0x05, 0xff], "no acceptable method");
        proxy.shutdown().await;
    }

    #[tokio::test]
    async fn a_socks5_wrong_password_is_rejected() {
        let (port, _server) = echo().await;
        let proxy = proxy_with(allow_loopback(port), ProxyLimits::default()).await;
        let proxy_port = proxy.endpoint().port().expect("port");
        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        client
            .write_all(&[0x05, 0x01, 0x02])
            .await
            .expect("greeting");
        let mut method = [0u8; 2];
        client.read_exact(&mut method).await.expect("method");
        assert_eq!(method, [0x05, 0x02]);

        // The right user with the wrong password: a check that only compared the
        // user name would let this through.
        let user = proxy
            .endpoint()
            .credential()
            .expect("credential")
            .userinfo()
            .split_once(':')
            .expect("user:pass")
            .0
            .to_owned();
        let mut login = vec![0x01, user.len() as u8];
        login.extend_from_slice(user.as_bytes());
        login.push(5);
        login.extend_from_slice(b"wrong");
        client.write_all(&login).await.expect("login");
        let mut status = [0u8; 2];
        client.read_exact(&mut status).await.expect("status");
        assert_eq!(status, [0x01, 0x01], "login rejected");
        // The connection is over: the caller learns nothing more through it.
        let mut rest = [0u8; 1];
        let read = client.read(&mut rest).await;
        assert!(matches!(read, Ok(0) | Err(_)), "{read:?}");
        proxy.shutdown().await;
    }

    /// The credential reaching a log would be equivalent to not having one: the
    /// environment it is written into is the only copy that matters.
    #[test]
    fn the_endpoint_debug_does_not_print_the_credential() {
        let credential = Credential::generate();
        let userinfo = credential.userinfo();
        let debug = format!("{credential:?}");
        assert_eq!(debug, "sandbox.Credential(<redacted>)");
        let endpoint = Endpoint::Loopback {
            port: 4321,
            credential,
        };
        let rendered = format!("{endpoint:?}");
        assert!(!rendered.contains(&userinfo), "{rendered}");
        assert!(
            !rendered.contains(userinfo.split(':').next().unwrap_or("")),
            "{rendered}"
        );
    }

    #[test]
    fn the_proxy_environment_carries_the_credential_in_the_userinfo() {
        let credential = Credential::generate();
        let userinfo = credential.userinfo();
        let env: std::collections::BTreeMap<_, _> =
            environment(4321, &credential).into_iter().collect();
        assert_eq!(
            env.get("HTTP_PROXY").map(String::as_str),
            Some(format!("http://{userinfo}@127.0.0.1:4321").as_str())
        );
        assert_eq!(
            env.get("ALL_PROXY").map(String::as_str),
            Some(format!("socks5h://{userinfo}@127.0.0.1:4321").as_str())
        );
        // `curl`, `git` and `pip` read userinfo from the URL; nothing else needs
        // to be configured for them to authenticate.
        assert_eq!(env.get("NO_PROXY").map(String::as_str), Some(""));
    }

    #[tokio::test]
    async fn a_unix_endpoint_is_bound_and_removed_on_shutdown() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("proxy.sock");
        let proxy = start(Config {
            policy: policy(NetMode::Proxy, vec![]),
            listen: Listen::Unix(path.clone()),
            audit: None,
            limits: ProxyLimits::default(),
        })
        .await
        .expect("proxy");
        assert_eq!(proxy.endpoint().socket(), Some(path.as_path()));
        assert!(path.exists(), "the socket is bound");
        proxy.shutdown().await;
        assert!(!path.exists(), "the socket is removed on shutdown");
    }
}
