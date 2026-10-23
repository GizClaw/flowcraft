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

use std::net::{IpAddr, SocketAddr};
use std::path::{Path, PathBuf};
use std::sync::Arc;

use flowcraft_error::{Error, Result};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::task::JoinHandle;

use super::matcher::Matcher;
use crate::contract::{NetMode, NetPolicy};

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
    /// The loopback port.
    Loopback(u16),
}

impl Endpoint {
    /// The socket path, when this is a unix endpoint.
    pub(crate) fn socket(&self) -> Option<&Path> {
        match self {
            Self::Unix(path) => Some(path),
            Self::Loopback(_) => None,
        }
    }

    /// The port, when this is a loopback endpoint.
    pub(crate) fn port(&self) -> Option<u16> {
        match self {
            Self::Unix(_) => None,
            Self::Loopback(port) => Some(*port),
        }
    }
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
pub(crate) fn environment(port: u16) -> Vec<(String, String)> {
    let http = format!("http://127.0.0.1:{port}");
    let socks = format!("socks5h://127.0.0.1:{port}");
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
}

impl std::fmt::Debug for Config {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.ProxyConfig")
            .field("mode", &self.policy.mode)
            .field("listen", &self.listen)
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
        return Err(Error::not_available(
            "sandbox: upstream proxy chaining is not implemented; drop `proxy` or use rules",
        ));
    }
    let matcher = Matcher::new(&config.policy)?;
    if matcher.is_empty() && config.policy.mode == NetMode::Proxy {
        // Nothing to deny and no upstream: a proxy that only forwards is still
        // worth having (it is the one hole), so this is allowed on purpose.
    }
    let mode = config.policy.mode;
    let audit = config.audit;
    let shared = Arc::new(Shared {
        matcher,
        mode,
        audit,
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
            Ok(Proxy {
                endpoint: Endpoint::Loopback(port),
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
}

impl Shared {
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
async fn read_head<S>(stream: &mut S, buffered: Vec<u8>) -> Result<Vec<u8>>
where
    S: AsyncRead + Unpin,
{
    let mut head = buffered;
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") && !head.ends_with(b"\n\n") {
        if head.len() >= MAX_HEAD {
            return Err(Error::validation(
                "sandbox: proxy request head is too large",
            ));
        }
        let read = stream.read(&mut byte).await.map_err(Error::from)?;
        if read == 0 {
            return Err(Error::validation(
                "sandbox: proxy request head is truncated",
            ));
        }
        head.push(byte[0]);
    }
    Ok(head)
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
    let mut parts = request.split_whitespace();
    let method = parts.next().unwrap_or_default().to_owned();
    let target = parts.next().unwrap_or_default().to_owned();

    if method.eq_ignore_ascii_case("CONNECT") {
        let (host, port) = split_authority(&target)?;
        let Some(endpoint) = authorize(&shared, &host, port) else {
            return deny(&mut stream, &shared, &host, port).await;
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
        return splice(stream, tunnel).await;
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
    let Ok(upstream) = connect(endpoint).await else {
        return fail(
            &mut stream,
            502,
            "the proxy could not reach the destination",
        )
        .await;
    };

    // Rebuild the head in origin form, dropping the proxy's own headers.
    let mut rewritten = format!("{method} {path} HTTP/1.1\r\n");
    for line in &headers {
        let name = line
            .split_once(':')
            .map(|(key, _)| key.trim())
            .unwrap_or_default();
        if name.eq_ignore_ascii_case("proxy-connection")
            || name.eq_ignore_ascii_case("proxy-authorization")
        {
            continue;
        }
        rewritten.push_str(line);
        rewritten.push_str("\r\n");
    }
    rewritten.push_str("\r\n");
    rewritten.push_str(&String::from_utf8_lossy(&head[0..0]));
    let mut upstream = upstream;
    upstream
        .write_all(rewritten.as_bytes())
        .await
        .map_err(Error::from)?;
    splice(stream, upstream).await
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
    let head = format!(
        "HTTP/1.1 {code} {}\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        if code == 403 {
            "Forbidden"
        } else {
            "Bad Gateway"
        },
        body.len()
    );
    stream
        .write_all(head.as_bytes())
        .await
        .map_err(Error::from)?;
    stream.shutdown().await.map_err(Error::from)
}

/// Copy both directions until either side closes.
async fn splice<A, B>(mut client: A, mut upstream: B) -> Result<()>
where
    A: AsyncRead + AsyncWrite + Unpin + Send,
    B: AsyncRead + AsyncWrite + Unpin + Send,
{
    let _ = tokio::io::copy_bidirectional(&mut client, &mut upstream).await;
    Ok(())
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
    if !greeting[2..2 + methods].contains(&0x00) {
        stream.write_all(&[0x05, 0xff]).await.map_err(Error::from)?;
        return Ok(());
    }
    stream.write_all(&[0x05, 0x00]).await.map_err(Error::from)?;

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
    splice(stream, upstream).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::{NetAction, NetRule};

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
            }))
            .expect_err("not implemented");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
        assert!(error.to_string().contains("not implemented"), "{error}");
    }

    fn policy(mode: NetMode, rules: Vec<NetRule>) -> NetPolicy {
        NetPolicy {
            mode,
            rules,
            ..NetPolicy::default()
        }
    }

    /// A loopback server that echoes a fixed greeting, so a tunnel is observable.
    async fn echo() -> (u16, JoinHandle<()>) {
        let listener = TcpListener::bind(("127.0.0.1", 0)).await.expect("bind");
        let port = listener.local_addr().expect("addr").port();
        let handle = tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                tokio::spawn(async move {
                    let mut buffer = [0u8; 64];
                    if let Ok(read) = stream.read(&mut buffer).await {
                        let _ = stream.write_all(&buffer[..read]).await;
                    }
                });
            }
        });
        (port, handle)
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
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");

        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = format!("CONNECT 127.0.0.1:{port} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n");
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
        })
        .await
        .expect("proxy");
        let proxy_port = proxy.endpoint().port().expect("port");

        let mut client = TcpStream::connect(("127.0.0.1", proxy_port))
            .await
            .expect("connect");
        let request = format!("CONNECT 127.0.0.1:{port} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n");
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
        })
        .await
        .expect("proxy");
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
        assert_eq!(method, [0x05, 0x00]);

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
        })
        .await
        .expect("proxy");
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
    async fn a_unix_endpoint_is_bound_and_removed_on_shutdown() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("proxy.sock");
        let proxy = start(Config {
            policy: policy(NetMode::Proxy, vec![]),
            listen: Listen::Unix(path.clone()),
            audit: None,
        })
        .await
        .expect("proxy");
        assert_eq!(proxy.endpoint().socket(), Some(path.as_path()));
        assert!(path.exists(), "the socket is bound");
        proxy.shutdown().await;
        assert!(!path.exists(), "the socket is removed on shutdown");
    }
}
