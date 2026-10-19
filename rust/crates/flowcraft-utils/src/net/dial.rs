//! The proxy's outbound side: where a connection to an allowed destination
//! actually goes.
//!
//! Three shapes, and core's `dialTarget` has all three: dial the destination
//! directly (allow-list mode), tunnel to it through an HTTP upstream with
//! `CONNECT`, or negotiate a SOCKS5 CONNECT with a SOCKS5 upstream.
//!
//! # Why the upstream's words are here
//!
//! An upstream is somebody else's server, so its refusals are messages a user
//! may see through a `502`. This module keeps those texts -- `netproxy: parse
//! upstream "..."`, `netproxy: unsupported upstream scheme "ftp"` -- and the
//! HTTP `CONNECT` request bytes, which are a contract of their own: an upstream
//! that requires `Proxy-Authorization` will not accept a request that spells the
//! header differently, and the header's presence depends only on whether the
//! URL carried userinfo.
//!
//! # The bytes that arrive with the `200`
//!
//! [`read_response_head`] stops at the end of the response head. An upstream
//! that writes its `200` and the first tunnel bytes in one write leaves those
//! bytes in the reader, and core used to drop them on the floor -- it returned
//! the raw socket and lost whatever `http.ReadResponse` had buffered. For a
//! target that speaks first (an SSH banner, a MySQL greeting) that is a tunnel
//! that never works. [`Dialer`] returns a [`Prefixed`] stream that replays them,
//! and `core/utils/net`'s `TestUpstreamConnectKeepsBytesAfterTheResponse` is the
//! test for the same fix on the Go side.

use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};
use std::time::Duration;

use base64::Engine;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, ReadBuf};
use tokio::net::TcpStream;

use super::socks5::{self, Credentials, Dialed};
use super::url;

/// Core's bound on how long an upstream may take to answer a `CONNECT`.
pub const UPSTREAM_RESPONSE_TIMEOUT: Duration = Duration::from_secs(30);
/// How long the TCP handshake to the destination (or the upstream) may take.
pub const CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
/// The largest response head an upstream may send before it is refused. Go's
/// `http.ReadResponse` has no such bound; a proxy that lets a stranger park a
/// task on an endless header line is not one worth porting faithfully.
pub const MAX_RESPONSE_HEAD: usize = 1 << 20;

/// Where proxy-mode traffic goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Upstream {
    /// No upstream: dial the destination directly.
    Direct,
    /// An HTTP proxy, reached with `CONNECT`.
    Http {
        /// The upstream's `host:port`, as the URL spelled it.
        authority: String,
        /// The URL's userinfo, when it had any.
        credentials: Option<Credentials>,
    },
    /// A SOCKS5 proxy.
    Socks5 {
        /// The upstream's `host:port`, as the URL spelled it.
        authority: String,
        /// The URL's userinfo, when it had any.
        credentials: Option<Credentials>,
    },
}

/// Why an upstream URL was refused, or a dial through one failed.
#[derive(Debug)]
pub enum Error {
    /// `url.Parse` refused the setting.
    ParseUrl {
        /// The URL as configured.
        raw: String,
        /// What the parser said.
        reason: String,
    },
    /// The URL parsed, but its scheme is not one this proxy speaks.
    UnsupportedScheme {
        /// The scheme it named.
        scheme: String,
    },
    /// The upstream answered the `CONNECT` with a status other than `200`.
    RefusedConnect {
        /// The status code.
        status: u16,
    },
    /// The upstream's response head was not one.
    MalformedResponse,
    /// The upstream's response head never ended.
    ResponseHeadTooLarge,
    /// The TCP handshake failed, or a read or write on the upstream did.
    Io(io::Error),
    /// The SOCKS5 negotiation with the upstream failed.
    Socks5(socks5::ClientError),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::ParseUrl { raw, reason } => {
                write!(f, "netproxy: parse upstream {raw:?}: {reason}")
            }
            Error::UnsupportedScheme { scheme } => {
                write!(f, "netproxy: unsupported upstream scheme {scheme:?}")
            }
            Error::RefusedConnect { status } => {
                write!(f, "netproxy: upstream refused CONNECT: {status}")
            }
            Error::MalformedResponse => {
                f.write_str("netproxy: malformed upstream CONNECT response")
            }
            Error::ResponseHeadTooLarge => {
                f.write_str("netproxy: upstream CONNECT response head too large")
            }
            Error::Io(err) => write!(f, "netproxy: {err}"),
            Error::Socks5(err) => write!(f, "netproxy: {err}"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Io(err) => Some(err),
            Error::Socks5(err) => Some(err),
            _ => None,
        }
    }
}

impl From<io::Error> for Error {
    fn from(err: io::Error) -> Error {
        Error::Io(err)
    }
}

impl From<socks5::ClientError> for Error {
    fn from(err: socks5::ClientError) -> Error {
        Error::Socks5(err)
    }
}

impl Upstream {
    /// Read a configured upstream: `Start`'s two steps, in `Start`'s order --
    /// parse, then switch on the scheme.
    ///
    /// The empty string is [`Upstream::Direct`], because that is what "no
    /// upstream configured" reaches `dialTarget` as.
    pub fn parse(raw: &str) -> Result<Upstream, Error> {
        if raw.is_empty() {
            return Ok(Upstream::Direct);
        }
        let parsed = url::parse(raw).map_err(|err| Error::ParseUrl {
            raw: raw.to_owned(),
            reason: err.to_string(),
        })?;
        let credentials = parsed.user.as_ref().map(|user| Credentials {
            username: user.username.clone(),
            password: user.password.clone().unwrap_or_default(),
        });
        match parsed.scheme.as_str() {
            "http" => Ok(Upstream::Http {
                authority: parsed.host,
                credentials,
            }),
            "socks5" => Ok(Upstream::Socks5 {
                authority: parsed.host,
                credentials,
            }),
            scheme => Err(Error::UnsupportedScheme {
                scheme: scheme.to_owned(),
            }),
        }
    }

    /// The `host:port` this dials, or `None` when it dials the destination
    /// itself.
    pub fn authority(&self) -> Option<&str> {
        match self {
            Upstream::Direct => None,
            Upstream::Http { authority, .. } | Upstream::Socks5 { authority, .. } => {
                Some(authority)
            }
        }
    }

    /// The userinfo the URL carried, if any.
    pub fn credentials(&self) -> Option<&Credentials> {
        match self {
            Upstream::Direct => None,
            Upstream::Http { credentials, .. } | Upstream::Socks5 { credentials, .. } => {
                credentials.as_ref()
            }
        }
    }
}

/// The `CONNECT` request an HTTP upstream receives, byte for byte.
///
/// The `Host` line repeats the authority because core writes it that way; the
/// `Proxy-Authorization` line exists exactly when the URL had userinfo, and it
/// carries the password even when the URL had none (`user:`).
pub fn connect_request(target: &str, credentials: Option<&Credentials>) -> String {
    let mut request = format!("CONNECT {target} HTTP/1.1\r\nHost: {target}");
    if let Some(credentials) = credentials {
        let pair = format!("{}:{}", credentials.username, credentials.password);
        request.push_str("\r\nProxy-Authorization: Basic ");
        request.push_str(&base64::engine::general_purpose::STANDARD.encode(pair));
    }
    request.push_str("\r\n\r\n");
    request
}

/// The status code in an HTTP response head, or why the head is not one.
///
/// Only the status line is read: `CONNECT` says nothing about what follows,
/// and core's only question is whether the status is `200`.
pub fn parse_response_head(head: &[u8]) -> Result<u16, Error> {
    let end = head
        .windows(2)
        .position(|window| window == b"\r\n")
        .ok_or(Error::MalformedResponse)?;
    let line = std::str::from_utf8(&head[..end]).map_err(|_| Error::MalformedResponse)?;
    let mut parts = line.split(' ');
    let version = parts.next().ok_or(Error::MalformedResponse)?;
    if !version.starts_with("HTTP/1.") {
        return Err(Error::MalformedResponse);
    }
    let status = parts.next().ok_or(Error::MalformedResponse)?;
    status.parse::<u16>().map_err(|_| Error::MalformedResponse)
}

/// Read an HTTP response head off `stream`, returning its status and the bytes
/// that arrived after it.
///
/// The leftover bytes are the tunnel's first bytes, not the head's: an upstream
/// that answers and immediately relays its target's greeting writes both in one
/// segment, and returning only the status would drop them.
pub async fn read_response_head<S>(stream: &mut S) -> Result<(u16, Vec<u8>), Error>
where
    S: AsyncRead + Unpin,
{
    let mut buffered = Vec::new();
    let mut chunk = [0u8; 4096];
    loop {
        let read = stream.read(&mut chunk).await?;
        if read == 0 {
            return Err(Error::MalformedResponse);
        }
        buffered.extend_from_slice(&chunk[..read]);
        if let Some(index) = head_end(&buffered) {
            let (head, rest) = buffered.split_at(index + 4);
            let status = parse_response_head(head)?;
            return Ok((status, rest.to_vec()));
        }
        if buffered.len() > MAX_RESPONSE_HEAD {
            return Err(Error::ResponseHeadTooLarge);
        }
    }
}

/// Where the head ends: the index of the `\r\n\r\n` that terminates it.
fn head_end(bytes: &[u8]) -> Option<usize> {
    bytes.windows(4).position(|window| window == b"\r\n\r\n")
}

/// Dial through an upstream, or directly.
#[derive(Debug, Clone)]
pub struct Dialer {
    upstream: Upstream,
    connect_timeout: Duration,
    response_timeout: Duration,
}

impl Dialer {
    /// A dialer over one upstream setting.
    pub fn new(upstream: Upstream) -> Dialer {
        Dialer {
            upstream,
            connect_timeout: CONNECT_TIMEOUT,
            response_timeout: UPSTREAM_RESPONSE_TIMEOUT,
        }
    }

    /// A dialer that goes straight to the destination.
    pub fn direct() -> Dialer {
        Dialer::new(Upstream::Direct)
    }

    /// The same dialer with other deadlines (core keeps these as variables so
    /// its tests can shrink them).
    pub fn with_timeouts(mut self, connect: Duration, response: Duration) -> Dialer {
        self.connect_timeout = connect;
        self.response_timeout = response;
        self
    }

    /// The upstream this dialer uses.
    pub fn upstream(&self) -> &Upstream {
        &self.upstream
    }

    /// Dial one destination, in the shape the connector wants.
    pub async fn dial(&self, target: &str) -> Result<Dialed, Error> {
        match &self.upstream {
            Upstream::Direct => {
                let stream = self.connect(target).await?;
                Ok(Dialed {
                    local: stream.local_addr().ok(),
                    stream: Box::new(stream),
                })
            }
            Upstream::Http {
                authority,
                credentials,
            } => {
                let mut stream = self.connect(authority).await?;
                let request = connect_request(target, credentials.as_ref());
                stream.write_all(request.as_bytes()).await?;
                let (status, leftover) =
                    tokio::time::timeout(self.response_timeout, read_response_head(&mut stream))
                        .await
                        .map_err(|_| {
                            Error::Io(io::Error::new(
                                io::ErrorKind::TimedOut,
                                "upstream CONNECT timed out",
                            ))
                        })??;
                if status != 200 {
                    return Err(Error::RefusedConnect { status });
                }
                Ok(Dialed {
                    local: stream.local_addr().ok(),
                    stream: Box::new(Prefixed::new(leftover, stream)),
                })
            }
            Upstream::Socks5 {
                authority,
                credentials,
            } => {
                let mut stream = self.connect(authority).await?;
                socks5::negotiate(&mut stream, target, credentials.as_ref()).await?;
                Ok(Dialed {
                    local: stream.local_addr().ok(),
                    stream: Box::new(stream),
                })
            }
        }
    }

    /// Connect to `address` under the dial deadline.
    async fn connect(&self, address: &str) -> Result<TcpStream, Error> {
        match tokio::time::timeout(self.connect_timeout, TcpStream::connect(address)).await {
            Ok(Ok(stream)) => {
                stream.set_nodelay(true)?;
                Ok(stream)
            }
            Ok(Err(err)) => Err(Error::Io(err)),
            Err(_) => Err(Error::Io(io::Error::new(
                io::ErrorKind::TimedOut,
                format!("dial {address}: timed out"),
            ))),
        }
    }
}

impl socks5::Connector for Dialer {
    fn connect<'a>(
        &'a self,
        target: &'a socks5::Target,
    ) -> super::BoxFuture<'a, io::Result<Dialed>> {
        Box::pin(async move {
            self.dial(&target.host_port()).await.map_err(|err| {
                // A SOCKS5 client is told the dial failed, never why; the text
                // is for the caller's log.
                io::Error::other(err.to_string())
            })
        })
    }
}

/// A stream that replays some bytes before it starts reading the real one.
///
/// It exists for exactly one reason (see the module docs): an upstream's `200`
/// and the tunnel's first bytes can arrive together, and the reader that saw
/// both is the only thing still holding the second.
pub struct Prefixed<S> {
    prefix: Vec<u8>,
    position: usize,
    inner: S,
}

impl<S> Prefixed<S> {
    /// `inner`, with `prefix` served first.
    pub fn new(prefix: Vec<u8>, inner: S) -> Prefixed<S> {
        Prefixed {
            prefix,
            position: 0,
            inner,
        }
    }

    /// The bytes still to be replayed.
    pub fn pending(&self) -> &[u8] {
        &self.prefix[self.position..]
    }
}

impl<S: AsyncRead + Unpin> AsyncRead for Prefixed<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        if self.position < self.prefix.len() {
            let pending = &self.prefix[self.position..];
            let take = pending.len().min(buffer.remaining());
            buffer.put_slice(&pending[..take]);
            self.position += take;
            return Poll::Ready(Ok(()));
        }
        Pin::new(&mut self.inner).poll_read(cx, buffer)
    }
}

impl<S: AsyncWrite + Unpin> AsyncWrite for Prefixed<S> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buffer)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}
