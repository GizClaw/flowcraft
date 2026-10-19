//! `net/mitm/engine.go`: the TLS terminator, and the decrypted layer under it.
//!
//! One [`Engine`] serves every tunnel of one proxy run. [`Engine::serve`] takes
//! a connection that has already been accepted -- the proxy has decided the host
//! is worth terminating and answered the `CONNECT` -- and everything under it is
//! here: the handshake, the h1/h2 dispatch ALPN chose, the hooks, and the
//! forwarding to the target.
//!
//! # What the client and the target each see
//!
//! The client talks to the engine over the certificate the [`Ca`] minted for the
//! tunnel's name, on whichever protocol it offered. The target always sees
//! **HTTP/1.1**: core's upstream `tls.Config` sets no `NextProtos`, so
//! `ForceAttemptHTTP2` never reaches the h2 branch, and the port's client TLS
//! config clears `alpn_protocols` for the same reason. That is a behavior a
//! target can observe, so `mitm-contract.json`'s `ambient.upstream_proto` pins
//! it.
//!
//! The target also sees what the transport adds on its own, and the engine adds
//! the same two things because they are behavior a target can key on rather
//! than an artifact of the implementation: `net/http` sends
//! `accept-encoding: gzip` to a request that asked for no encoding and decodes
//! exactly what it asked for, and it names itself `Go-http-client/1.1` on a
//! request that names no user agent. The port does both -- a client that sends
//! no `accept-encoding` still receives a body it can read, and a target that
//! branches on the user agent sees what core's transport showed it. The name is
//! the transport's, not this crate's; a deployment that wants its own identity
//! on the wire needs a knob core does not have, so it does not exist here
//! either.
//!
//! # What the hooks see, and why the shape is constructed
//!
//! Core's `MITMRequestInfo` is `Method`/`Scheme`/`Host`/`Path`/`Header` plus an
//! optional body, and the port builds every one of those from the request rather
//! than handing the stack's own view through -- because the two stacks differ
//! exactly where the contract compares:
//!
//! - `Host` is a **field**, not a header. Go's server moves it out of
//!   `Request.Header` (from `Host:` on h1, from `:authority` on h2); hyper
//!   leaves the h1 header in the map, so `reported` drops it.
//! - `Transfer-Encoding` never appears: Go moves it into
//!   `Request.TransferEncoding`. `Content-Length` does appear, which is the half
//!   a port gets wrong by dropping both framing headers or neither.
//! - `Body` is `None` when the hook was given no body and `Some(empty)` when it
//!   was given an empty one. That difference is the only evidence that a body
//!   was inspected at all, so it is not a detail.
//! - A body is read only when inspection is on *and* the message declares a
//!   length *and* that length is within the cap. Everything else is streamed
//!   through untouched: a hook that is given no body must not cost the target a
//!   byte, which is what `post-over-the-cap` exists to pin.
//!
//! # What is deliberately not here
//!
//! Two things core has are a caller's decision in the port. **System roots**: a
//! run inside a sandbox, and every conformance row, pins a trust store, so
//! `roots` is a parameter and loading the platform store is the deploy slice's
//! problem rather than a `nil` the engine fills in. **Deadlines**: hyper bounds
//! reading a request head and the pool bounds idleness, but a whole upstream
//! round trip has no deadline, because its future is cancelled when the client
//! goes away -- the property core's comment about request contexts is after.
//! The numeric constants core keeps are still here, named, so the values that
//! belong to a port-level test are written down.

use std::collections::VecDeque;
use std::io;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, Instant};

use async_compression::tokio::bufread::GzipDecoder;
use flowcraft_contract::error::{Error, ErrorKind};
use http_body_util::combinators::BoxBody;
use http_body_util::{BodyExt, Full};
use hyper::body::{Body as HttpBody, Bytes, Frame, Incoming, SizeHint};
use hyper::client::conn::http1::SendRequest;
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::http::request::Parts;
use hyper::http::{Method, Request, Response, StatusCode};
use hyper::server::conn::{http1, http2};
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use rustls::pki_types::ServerName;
use rustls::{ClientConfig, RootCertStore};
use tokio::io::{AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};
use tokio_rustls::{TlsAcceptor, TlsConnector};

use super::ca::Ca;
use super::{Hooks, Selection};
use crate::net::BoxFuture;
use crate::net::dial::Dialer;
use crate::net::policy::MitmPolicy;
use crate::net::socks5::Stream;

/// The protocols the terminator offers, in the order core offers them.
///
/// rustls picks by the server's order here, which is what `tls.Server` does with
/// `NextProtos{"h2", "http/1.1"}`, so a client that prefers h1 does not get it.
pub const ALPN: &[&[u8]] = &[b"h2", b"http/1.1"];

/// What a policy that names no cap gets (`mitm.DefaultMaxBodyBytes`).
pub const DEFAULT_MAX_BODY_BYTES: u64 = 64 << 10;

/// How long a client may take to send one request head
/// (`mitmReadHeaderTimeout`).
pub const READ_HEADER_TIMEOUT: Duration = Duration::from_secs(30);

/// How long the upstream dial and TLS handshake together may take
/// (`mitmTLSHandshakeTimeout`).
pub const TLS_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

/// How long an upstream connection may sit idle in the pool before it is
/// dropped (`mitmIdleConnTimeout`).
pub const IDLE_CONN_TIMEOUT: Duration = Duration::from_secs(90);

/// The bound on one request head (`mitmMaxRequestHeaderBytes`), which is
/// `http.Server.MaxHeaderBytes`.
pub const MAX_REQUEST_HEADER_BYTES: usize = 1 << 20;

/// What `net/http` reads past `MaxHeaderBytes` before it gives up: its reader
/// allows 4 KiB of slack, so this is where a request is actually refused.
pub const HEADER_SIZE_SLACK: usize = 4 << 10;

/// How many idle upstream connections one tunnel's pool keeps
/// (`MaxIdleConnsPerHost`; a tunnel's transport only ever has the one host).
const MAX_IDLE_CONNS: usize = 2;

/// How much of a body of unknown length is buffered before the writer decides
/// to frame it as chunked. `net/http` uses a 2 KiB buffer for exactly this, and
/// `response-chunked` is the row that pins the result.
const PEEK_BODY_BYTES: u64 = 2048;

/// How large a chunk the decoded body reads at a time.
const READ_CHUNK: usize = 8 << 10;

/// How much compressed data the decoder's pipe holds.
const DECODE_PIPE: usize = 8 << 10;

/// The sentence a refused request is answered with (core's `errBlocked`).
const BLOCKED: &str = "mitm: blocked by hook";

/// The sentence a failed round trip is answered with.
const UPSTREAM_FAILED: &str = "mitm: upstream request failed";

/// The user agent `net/http` puts on a request that names none, reproduced
/// because `gzip-the-client-did-not-ask-for` pins it on the target's side.
const DEFAULT_USER_AGENT: &str = "Go-http-client/1.1";

/// What one tunnel terminates: the name the leaf is minted for -- which is also
/// the upstream's SNI -- and the address the target is dialed at, which a
/// caller's dialer may ignore.
struct Tunnel {
    server_name: String,
    dial: String,
}

/// Where a decrypted request's upstream connection comes from.
///
/// Core spells this as a `func(context.Context, string) (net.Conn, error)`
/// handed to `mitm.New`: the proxy's policy-aware dialer, returning a raw
/// connection so the engine owns the TLS. A trait rather than a closure because
/// the engine holds it for the life of the process.
pub trait UpstreamDialer: Send + Sync {
    /// A pre-TLS connection to `hostport`.
    fn dial<'a>(&'a self, hostport: &'a str) -> BoxFuture<'a, Result<Box<dyn Stream>, Error>>;
}

impl UpstreamDialer for Dialer {
    fn dial<'a>(&'a self, hostport: &'a str) -> BoxFuture<'a, Result<Box<dyn Stream>, Error>> {
        Box::pin(async move {
            let dialed = Dialer::dial(self, hostport)
                .await
                .map_err(|err| internal(format!("mitm: dial {hostport}: {err}")))?;
            Ok(dialed.stream)
        })
    }
}

impl<T: UpstreamDialer + ?Sized> UpstreamDialer for Arc<T> {
    fn dial<'a>(&'a self, hostport: &'a str) -> BoxFuture<'a, Result<Box<dyn Stream>, Error>> {
        (**self).dial(hostport)
    }
}

/// Core's `MITMRequestInfo`: one decrypted request, as a hook sees it.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RequestInfo {
    /// The method, as the client spelled it.
    pub method: String,
    /// The scheme. Always `https` here: a terminated tunnel is one.
    pub scheme: String,
    /// The authority the client named.
    pub host: String,
    /// The request target, query string included.
    pub path: String,
    /// The headers, without the authority and the framing header (see the
    /// module docs).
    pub header: HeaderMap,
    /// `None` when the hook was given no body, `Some(empty)` when it was given
    /// an empty one.
    pub body: Option<Bytes>,
}

/// Core's `MITMResponseInfo`: one decrypted response, as a hook sees it.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ResponseInfo {
    /// The status code the target answered with.
    pub status: u16,
    /// The headers, without `transfer-encoding`.
    pub header: HeaderMap,
    /// `None` when the hook was given no body, `Some(empty)` when it was given
    /// an empty one.
    pub body: Option<Bytes>,
}

/// The body every message leaving this module carries: its `size_hint` is what
/// decides the client's framing, so the type has to be able to say "unknown".
type Out = BoxBody<Bytes, OutError>;

/// The erased error `Out` carries. `hyper` only asks that it be printable.
type OutError = Box<dyn std::error::Error + Send + Sync>;

/// How a message declared its body.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Framing {
    /// No framing header at all: no body, or an h2 message.
    None,
    /// A declared length.
    Length(u64),
    /// `Transfer-Encoding: chunked`, so the length is unknown.
    Chunked,
}

/// A body after the inspection decision.
struct Inspected {
    /// What the hook is shown, if it is shown anything.
    hook_body: Option<Bytes>,
    /// What goes on: a local copy when the body was read, so it can be replayed
    /// byte for byte, and the caller's own body otherwise.
    body: Out,
    /// Whether `body` is entirely in memory. A local body frees the upstream
    /// connection as soon as the request is written; a streamed one holds it
    /// until it ends.
    local: bool,
}

/// One decrypted request on its way upstream: what the client said, with the
/// body already decided.
///
/// A struct rather than `forward`'s argument list because the five travel
/// together -- they are the request's parts, split by `handle` so the hook can
/// be shown them.
struct Inbound<'a> {
    method: Method,
    path: &'a str,
    authority: &'a str,
    head: &'a HeaderMap,
    body: Out,
}

/// One engine, shared by every tunnel of one proxy run.
pub struct Engine {
    /// The hooks, when a caller installed any. `None` is `no-hooks`, whose whole
    /// contract is an empty trace.
    hooks: Option<Arc<dyn Hooks>>,
    /// The authority leaves are minted from.
    ca: Ca,
    /// How the target's certificate is verified.
    tls: Arc<ClientConfig>,
    /// The policy-aware dialer, pre-TLS.
    dial: Arc<dyn UpstreamDialer>,
    /// The compiled host selection, so a proxy can ask one object everything.
    selection: Selection,
    /// The cap on an inspected body, in bytes.
    cap: u64,
    /// Whether bodies are inspected at all.
    inspect_bodies: bool,
}

/// The idle upstream connections of one tunnel, oldest first.
///
/// One per tunnel, not one per engine: core builds a transport per `Serve` and
/// closes its idle connections when the tunnel ends, and a connection is only
/// ever good for the target it was opened to. A pool shared by two tunnels
/// would hand one tunnel's connection to the other's target.
#[derive(Default)]
struct Pool {
    idle: Mutex<VecDeque<Idle>>,
}

/// One pooled connection, with the moment it went idle.
struct Idle {
    at: Instant,
    send: SendRequest<Out>,
}

impl Pool {
    /// Take an idle connection, dropping the ones that went stale.
    fn take(&self) -> Option<SendRequest<Out>> {
        let mut idle = self.lock();
        while let Some(connection) = idle.pop_front() {
            if connection.at.elapsed() > IDLE_CONN_TIMEOUT || connection.send.is_closed() {
                continue;
            }
            return Some(connection.send);
        }
        None
    }

    /// Hand a connection back, if it is still usable and the pool has room.
    fn put(&self, send: SendRequest<Out>) {
        if send.is_closed() {
            return;
        }
        let mut idle = self.lock();
        if idle.len() >= MAX_IDLE_CONNS {
            idle.pop_front();
        }
        idle.push_back(Idle {
            at: Instant::now(),
            send,
        });
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, VecDeque<Idle>> {
        // A panic elsewhere does not make a pool of connections wrong, only
        // stale, and a stale connection is dropped on the next `take`.
        self.idle
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }
}

impl std::fmt::Debug for Engine {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Engine")
            .field("hooks", &self.hooks.is_some())
            .field("cap", &self.cap)
            .field("inspect_bodies", &self.inspect_bodies)
            .field("selection", &self.selection)
            .finish_non_exhaustive()
    }
}

impl Engine {
    /// Build an engine: the authority, the selection, the upstream roots and the
    /// dialer.
    ///
    /// `roots` is what the target's certificate is verified against -- core's
    /// `roots *x509.CertPool`, with the difference that "the system store" is a
    /// caller's decision here rather than a `nil` the engine fills in (see the
    /// module docs).
    ///
    /// The two host lists are compiled here, in core's order, so a pattern that
    /// does not parse is refused at construction rather than at the first
    /// `CONNECT`.
    pub fn new(
        policy: &MitmPolicy,
        hooks: Option<Arc<dyn Hooks>>,
        dial: Arc<dyn UpstreamDialer>,
        roots: Arc<RootCertStore>,
    ) -> Result<Engine, Error> {
        let selection = Selection::new(policy)?;
        let cap = if policy.max_body_bytes <= 0 {
            DEFAULT_MAX_BODY_BYTES
        } else {
            policy.max_body_bytes as u64
        };
        Ok(Engine {
            hooks,
            ca: Ca::new()?,
            tls: Arc::new(client_config(&roots)?),
            dial,
            selection,
            cap,
            inspect_bodies: policy.inspect_bodies,
        })
    }

    /// The temporary root in PEM form, for bundle injection into the sandbox.
    pub fn pem(&self) -> &str {
        self.ca.pem()
    }

    /// The same root as a certificate, which is what a client that has to be
    /// handed a trust store rather than a file needs -- core's `PEM` is the
    /// sandbox's half of the same fact.
    pub fn root(&self) -> rustls::pki_types::CertificateDer<'static> {
        self.ca.root().clone()
    }

    /// `Engine.ShouldMITM`: is this host terminated, or tunnelled through?
    pub fn should_mitm(&self, host: &str) -> bool {
        self.selection.should_mitm(host)
    }

    /// Terminate TLS on `client` for `server_name` and serve the decrypted
    /// traffic until the client goes away.
    ///
    /// `dial` is the `host:port` upstream requests are sent to -- core's
    /// `dialHostport`, which the caller knows and the request does not name. A
    /// client that sends `Host: other.test` through a tunnel opened for
    /// `example.test` is forwarded *to* `example.test`'s address *with*
    /// `other.test` as the authority, which is what `host-is-not-the-sni` pins.
    ///
    /// Returns `Ok` when the connection ended, which includes the client
    /// vanishing mid-request: core treats `net.ErrClosed`, `http.ErrServerClosed`
    /// and the `431` its own server writes the same way. Under h2 nothing is
    /// reported at all, because core's h2 path never sees the session's error --
    /// see `net`'s module docs.
    pub async fn serve<S>(
        self: Arc<Self>,
        client: S,
        server_name: &str,
        dial: &str,
    ) -> Result<(), Error>
    where
        S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
    {
        let config = self.ca.server_config(server_name, ALPN)?;
        let stream = TlsAcceptor::from(config)
            .accept(client)
            .await
            .map_err(|err| internal(format!("mitm: client handshake: {err}")))?;
        let tunnel = Tunnel {
            server_name: server_name.to_owned(),
            dial: dial.to_owned(),
        };
        let negotiated_h2 = stream.get_ref().1.alpn_protocol() == Some(b"h2");
        // The tunnel's own idle connections: core's transport lives and dies
        // with `Serve` in exactly the same way, and a connection is only good
        // for the target it was opened to.
        let pool = Arc::new(Pool::default());
        let service = {
            let engine = Arc::clone(&self);
            let pool = Arc::clone(&pool);
            hyper::service::service_fn(move |request: Request<Incoming>| {
                let engine = Arc::clone(&engine);
                let pool = Arc::clone(&pool);
                let name = tunnel.server_name.clone();
                let dial = tunnel.dial.clone();
                async move {
                    let tunnel = Tunnel {
                        server_name: name,
                        dial,
                    };
                    Ok::<_, std::convert::Infallible>(engine.handle(request, &tunnel, &pool).await)
                }
            })
        };
        let io = TokioIo::new(stream);
        if negotiated_h2 {
            // Core's h2 path runs the session in `http.Server`'s own goroutine
            // and never returns its error: the only error `serveH2` can report
            // is its listener's, and that listener hands out one connection.
            // The port keeps the same surface -- a client that vanishes is an
            // event with no caller to tell.
            let _ = http2::Builder::new(TokioExecutor::new())
                .serve_connection(io, service)
                .await;
            return Ok(());
        }
        let mut builder = http1::Builder::new();
        builder
            .max_header_size(MAX_REQUEST_HEADER_BYTES + HEADER_SIZE_SLACK)
            .header_read_timeout(READ_HEADER_TIMEOUT)
            .timer(TokioTimer::new());
        match builder.serve_connection(io, service).await {
            Ok(()) => Ok(()),
            Err(err) if ends_a_connection(&err) => Ok(()),
            Err(err) => Err(internal(format!("mitm: serve: {err}"))),
        }
    }

    /// One decrypted request: inspect, hook, forward, inspect, answer.
    async fn handle(
        self: &Arc<Self>,
        request: Request<Incoming>,
        tunnel: &Tunnel,
        pool: &Arc<Pool>,
    ) -> Response<Out> {
        let (parts, incoming) = request.into_parts();
        let framing = framing_of(&parts.headers);
        let authority = authority_of(&parts);
        let path = parts
            .uri
            .path_and_query()
            .map(|target| target.as_str().to_owned())
            .unwrap_or_else(|| "/".to_owned());
        let head = reported(&parts.headers);

        let body = if framing == Framing::None {
            empty()
        } else {
            streamed(incoming)
        };
        let inspected = self.inspected(framing, body).await;

        if let Some(hooks) = &self.hooks
            && hooks
                .on_request(&RequestInfo {
                    method: parts.method.as_str().to_owned(),
                    scheme: "https".to_owned(),
                    host: authority.clone(),
                    path: path.clone(),
                    header: head.clone(),
                    body: inspected.hook_body,
                })
                .is_err()
        {
            return refusal(StatusCode::FORBIDDEN, BLOCKED);
        }

        match self
            .forward(
                Inbound {
                    method: parts.method,
                    path: &path,
                    authority: &authority,
                    head: &head,
                    body: inspected.body,
                },
                tunnel,
                pool,
            )
            .await
        {
            Ok(response) => response,
            Err(Refused::Blocked) => refusal(StatusCode::FORBIDDEN, BLOCKED),
            Err(Refused::Upstream(_err)) => refusal(StatusCode::BAD_GATEWAY, UPSTREAM_FAILED),
        }
    }

    /// Core's `forward`: the outbound round trip, the response hook, and the
    /// answer in the shape the client's protocol wants.
    async fn forward(
        self: &Arc<Self>,
        inbound: Inbound<'_>,
        tunnel: &Tunnel,
        pool: &Arc<Pool>,
    ) -> Result<Response<Out>, Refused> {
        // `net/http` asks for gzip on a request that asked for nothing, and
        // decodes what it asked for. The two halves travel together: the decoded
        // body has no `content-encoding` and no length, which is exactly what
        // core's rows show the hooks and the client.
        let Inbound {
            method,
            path,
            authority,
            head,
            body,
        } = inbound;
        let asked_gzip = !head.contains_key(header::ACCEPT_ENCODING);
        let request = upstream_request(method, path, authority, head, body, asked_gzip)
            .map_err(Refused::Upstream)?;
        let (sender, response) = self
            .send(request, tunnel, pool)
            .await
            .map_err(Refused::Upstream)?;
        let (parts, incoming) = response.into_parts();
        let mut head = reported(&parts.headers);
        let mut framing = framing_of(&parts.headers);
        let body = if asked_gzip && is_gzip(&head) {
            head.remove(header::CONTENT_ENCODING);
            head.remove(header::CONTENT_LENGTH);
            framing = Framing::None;
            decoded(incoming)
        } else {
            streamed(incoming)
        };

        let inspected = self.inspected(framing, body).await;
        if let Some(hooks) = &self.hooks
            && hooks
                .on_response(&ResponseInfo {
                    status: parts.status.as_u16(),
                    header: head.clone(),
                    body: inspected.hook_body,
                })
                .is_err()
        {
            return Err(Refused::Blocked);
        }
        let (head, body, local) = presented(head, inspected.body, framing, inspected.local).await;
        // A body that is entirely in memory frees the connection now; one that
        // is still arriving gives it back when it ends.
        let body = if local {
            pool.put(sender);
            body
        } else {
            pooled(body, Arc::clone(pool), sender)
        };
        let mut response = Response::builder().status(parts.status);
        if let Some(headers) = response.headers_mut() {
            *headers = head;
        }
        response
            .body(body)
            .map_err(|err| Refused::Upstream(internal(format!("mitm: response: {err}"))))
    }

    /// Send one request upstream, reusing a pooled connection when there is one.
    ///
    /// The sender comes back with the response because only the caller knows
    /// whether the body will be read out, and that is what decides whether the
    /// connection is reusable.
    ///
    /// A pooled connection that turns out to be dead is not retried: core's
    /// transport retries an idempotent request on a fresh connection, and the
    /// port answers `502` instead. Retrying needs the request body, which is a
    /// stream by then.
    async fn send(
        &self,
        request: Request<Out>,
        tunnel: &Tunnel,
        pool: &Pool,
    ) -> Result<(SendRequest<Out>, Response<Incoming>), Error> {
        let mut sender = match pool.take() {
            Some(sender) => sender,
            None => self.connect(tunnel).await?,
        };
        let response = sender
            .send_request(request)
            .await
            .map_err(|err| internal(format!("mitm: upstream request: {err}")))?;
        Ok((sender, response))
    }

    /// A fresh upstream connection: dial, verify the target, and speak h1.
    async fn connect(&self, tunnel: &Tunnel) -> Result<SendRequest<Out>, Error> {
        let server_name = ServerName::try_from(tunnel.server_name.clone()).map_err(|err| {
            internal(format!(
                "mitm: upstream server name {:?}: {err}",
                tunnel.server_name
            ))
        })?;
        let connector = TlsConnector::from(Arc::clone(&self.tls));
        // Core puts one deadline around the dial and the handshake together;
        // this bounds each phase by the same number.
        let stream = tokio::time::timeout(TLS_HANDSHAKE_TIMEOUT, self.dial.dial(&tunnel.dial))
            .await
            .map_err(|_| internal("mitm: upstream dial timed out"))??;
        let stream = tokio::time::timeout(
            TLS_HANDSHAKE_TIMEOUT,
            connector.connect(server_name, stream),
        )
        .await
        .map_err(|_| internal("mitm: upstream handshake timed out"))?
        .map_err(|err| internal(format!("mitm: upstream handshake: {err}")))?;
        let (sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
            .await
            .map_err(|err| internal(format!("mitm: upstream handshake: {err}")))?;
        // The connection future drives reads and writes; nobody else holds it.
        tokio::spawn(async move {
            let _ = connection.await;
        });
        Ok(sender)
    }

    /// Core's `inspectRequest`/`inspectResponse`, body half: a body is read only
    /// when inspection is on, the message declares a length, and that length is
    /// within the cap.
    ///
    /// Anything else is passed through untouched -- a length over the cap,
    /// chunked framing, a decoded body -- so the target still receives every
    /// byte of a request the hook was not shown.
    async fn inspected(&self, framing: Framing, body: Out) -> Inspected {
        if !self.inspect_bodies {
            return Inspected {
                hook_body: None,
                body,
                local: false,
            };
        }
        match framing {
            // Go's `req.Body` is `http.NoBody` for a request without one, and
            // `io.ReadAll` of that is an empty *non-nil* slice: `""`, not null.
            Framing::None => Inspected {
                hook_body: Some(Bytes::new()),
                body,
                local: false,
            },
            Framing::Length(len) if len <= self.cap => {
                let mut body = body;
                let (prefix, complete) = read_within(&mut body, self.cap).await;
                if complete {
                    Inspected {
                        hook_body: Some(prefix.clone()),
                        body: exact(prefix),
                        local: true,
                    }
                } else {
                    // The declared length lied. Core puts the bytes it took
                    // back and inspects nothing; the body stays whole.
                    Inspected {
                        hook_body: None,
                        body: paired(prefix, body),
                        local: false,
                    }
                }
            }
            _ => Inspected {
                hook_body: None,
                body,
                local: false,
            },
        }
    }
}

/// Why `forward` produced no response.
enum Refused {
    /// A hook refused the traffic: `403` and core's sentence.
    Blocked,
    /// The round trip failed: `502` and core's sentence. The error is kept for a
    /// caller that logs; nothing on the wire is derived from it, which is why
    /// core's rows show one sentence for a refused dial and for a rejected
    /// certificate alike.
    Upstream(Error),
}

/// The client's view of a response: a head it can rely on, and a body whose
/// `size_hint` says how to frame it.
///
/// `net/http` measures a body of unknown length up to its 2 KiB buffer and
/// writes a `content-length` when the whole body fits, which is why a chunked
/// answer from the target reaches core's client as `framing: length`. The
/// writer's decision is not the engine's, but it is one the port has to make the
/// same way or the same response is delimited differently on the wire.
///
/// `local` comes back with the body: a peeked body that fit the buffer is in
/// memory and frees its connection, one that did not is still streaming.
async fn presented(
    head: HeaderMap,
    body: Out,
    framing: Framing,
    local: bool,
) -> (HeaderMap, Out, bool) {
    let mut head = head;
    if let Framing::Length(len) = framing {
        set_length(&mut head, len);
        return (head, body, local);
    }
    let mut body = body;
    let (prefix, complete) = read_within(&mut body, PEEK_BODY_BYTES).await;
    if complete {
        set_length(&mut head, prefix.len() as u64);
        (head, exact(prefix), true)
    } else {
        (head, paired(prefix, body), false)
    }
}

/// Build the request the target receives.
///
/// The URI stays origin-form and the authority becomes the `Host` header, which
/// is the only way a tunnel opened for one name can carry a request for another
/// (`host-is-not-the-sni`). The framing headers are dropped and left to hyper,
/// which writes them from the body: a `Full` body gets a `content-length`, and a
/// body of unknown length gets `transfer-encoding: chunked` for the methods that
/// can have one.
///
/// The two defaults `net/http` adds are added here, and only where the client
/// left them off: a client's own `accept-encoding` or `user-agent` is passed
/// through untouched.
fn upstream_request(
    method: Method,
    path: &str,
    authority: &str,
    head: &HeaderMap,
    body: Out,
    asked_gzip: bool,
) -> Result<Request<Out>, Error> {
    let mut builder = Request::builder().method(method).uri(path);
    let headers = builder
        .headers_mut()
        .ok_or_else(|| internal("mitm: the upstream request builder refused its own headers"))?;
    for name in head.keys() {
        if name == header::HOST
            || name == header::CONTENT_LENGTH
            || name == header::TRANSFER_ENCODING
        {
            continue;
        }
        for value in head.get_all(name) {
            headers.append(name.clone(), value.clone());
        }
    }
    headers.insert(
        header::HOST,
        HeaderValue::from_str(authority)
            .map_err(|err| internal(format!("mitm: authority {authority:?}: {err}")))?,
    );
    if asked_gzip {
        headers.insert(header::ACCEPT_ENCODING, HeaderValue::from_static("gzip"));
    }
    if !head.contains_key(header::USER_AGENT) {
        headers.insert(
            header::USER_AGENT,
            HeaderValue::from_static(DEFAULT_USER_AGENT),
        );
    }
    builder
        .body(body)
        .map_err(|err| internal(format!("mitm: upstream request: {err}")))
}

/// Verify the target with the configured roots, over TLS 1.2 and up, and offer
/// no ALPN -- core's upstream `tls.Config` sets no `NextProtos`, so the
/// `http.Transport` never takes its h2 branch whatever the target would prefer.
fn client_config(roots: &Arc<RootCertStore>) -> Result<ClientConfig, Error> {
    let mut config =
        ClientConfig::builder_with_provider(Arc::new(rustls::crypto::ring::default_provider()))
            .with_safe_default_protocol_versions()
            .map_err(|err| internal(format!("mitm: upstream protocol versions: {err}")))?
            .with_root_certificates(RootCertStore::clone(roots))
            .with_no_client_auth();
    config.alpn_protocols.clear();
    Ok(config)
}

/// The headers a hook is shown: hyper's map minus the authority and the framing.
///
/// `host` becomes [`RequestInfo::host`], and `transfer-encoding` never survives
/// core's server either; `content-length` does, on both sides, which is why it
/// is not in this list.
fn reported(headers: &HeaderMap) -> HeaderMap {
    let mut out = HeaderMap::new();
    for name in headers.keys() {
        if name == header::HOST || name == header::TRANSFER_ENCODING {
            continue;
        }
        for value in headers.get_all(name) {
            out.append(name.clone(), value.clone());
        }
    }
    out
}

/// The authority a request names, as a field. h1 carries it in `Host`, h2 in
/// `:authority`, which hyper leaves in the URI rather than in the headers.
fn authority_of(parts: &Parts) -> String {
    if let Some(authority) = parts.uri.authority() {
        return authority.as_str().to_owned();
    }
    parts
        .headers
        .get(header::HOST)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_owned()
}

/// How a message declared its body.
fn framing_of(headers: &HeaderMap) -> Framing {
    for value in headers.get_all(header::TRANSFER_ENCODING) {
        let chunked = value
            .to_str()
            .map(|value| {
                value
                    .split(',')
                    .any(|coding| coding.trim().eq_ignore_ascii_case("chunked"))
            })
            .unwrap_or(false);
        if chunked {
            return Framing::Chunked;
        }
    }
    match headers
        .get(header::CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.trim().parse::<u64>().ok())
    {
        Some(len) => Framing::Length(len),
        None => Framing::None,
    }
}

/// Whether the target answered with gzip, which the transport decodes only when
/// the transport asked for it.
fn is_gzip(headers: &HeaderMap) -> bool {
    headers
        .get(header::CONTENT_ENCODING)
        .and_then(|value| value.to_str().ok())
        .map(|value| value.trim().eq_ignore_ascii_case("gzip"))
        .unwrap_or(false)
}

/// Record a length on a head the caller is about to frame.
fn set_length(head: &mut HeaderMap, len: u64) {
    if let Ok(value) = HeaderValue::from_str(&len.to_string()) {
        head.insert(header::CONTENT_LENGTH, value);
    }
}

/// What core's `http.Error` writes: the status, a plain-text head that asks not
/// to be sniffed, and the sentence with the newline `http.Error` appends. The
/// byte count of that body (22 for a refusal, 30 for a failure) is part of the
/// contract, so the sentence lives in one place.
fn refusal(status: StatusCode, message: &str) -> Response<Out> {
    let body = format!("{message}\n");
    let mut response = Response::builder().status(status);
    if let Some(head) = response.headers_mut() {
        head.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("text/plain; charset=utf-8"),
        );
        head.insert(
            header::X_CONTENT_TYPE_OPTIONS,
            HeaderValue::from_static("nosniff"),
        );
        set_length(head, body.len() as u64);
    }
    response
        .body(exact(Bytes::from(body)))
        .unwrap_or_else(|_| Response::new(empty()))
}

/// Hyper's way of saying the connection ended rather than broke: the client went
/// away mid-request, the connection was closed, the peer stopped answering, or
/// the server refused a head it had already answered `431` to. Core treats all
/// of them as a normal end (`net.ErrClosed`, `http.ErrServerClosed`, and the
/// `431` path), and a shutdown that could not be written is the peer having
/// gone first.
fn ends_a_connection(err: &hyper::Error) -> bool {
    err.is_incomplete_message()
        || err.is_closed()
        || err.is_canceled()
        || err.is_parse_too_large()
        || err.is_shutdown()
}

fn internal(message: impl Into<String>) -> Error {
    Error::new(ErrorKind::Internal, message.into())
}

/// Read at most `limit` bytes of a body, and say whether that was all of it.
///
/// Stopping at the limit is the point: a body that is still going after `limit`
/// bytes is one the caller must put back together rather than truncate.
async fn read_within<B>(body: &mut B, limit: u64) -> (Bytes, bool)
where
    B: HttpBody<Data = Bytes> + Unpin,
{
    let mut collected: Vec<u8> = Vec::new();
    while let Some(frame) = body.frame().await {
        let Ok(frame) = frame else {
            return (Bytes::from(collected), false);
        };
        if let Ok(data) = frame.into_data() {
            collected.extend_from_slice(&data);
            if collected.len() as u64 > limit {
                return (Bytes::from(collected), false);
            }
        }
    }
    (Bytes::from(collected), true)
}

/// A body that is already in hand.
fn exact(bytes: Bytes) -> Out {
    Full::new(bytes).map_err(never).boxed()
}

/// A body that is known to be empty: no `content-length`, no chunked framing.
fn empty() -> Out {
    exact(Bytes::new())
}

/// A body that is still arriving.
fn streamed(body: Incoming) -> Out {
    body.map_err(|err| Box::new(err) as OutError).boxed()
}

/// A consumed prefix followed by the rest: `io.MultiReader`'s shape, and the
/// reason a body read for inspection can still reach the target whole.
fn paired(prefix: Bytes, rest: Out) -> Out {
    Pair::new(exact(prefix), rest).boxed()
}

/// The decoder `net/http` would have run: gzip, streamed, multi-member as Go's
/// `gzip.Reader` is by default.
fn decoded(body: Incoming) -> Out {
    let (mut compressed, plain) = tokio::io::duplex(DECODE_PIPE);
    tokio::spawn(async move {
        let mut body = body;
        while let Some(frame) = body.frame().await {
            let Ok(frame) = frame else { break };
            if let Ok(data) = frame.into_data()
                && compressed.write_all(&data).await.is_err()
            {
                break;
            }
        }
        // Closing the pipe is what tells the decoder the stream ended; without
        // it a truncated answer would be read as an unfinished one.
        let _ = compressed.shutdown().await;
    });
    let mut decoder = GzipDecoder::new(BufReader::new(plain));
    decoder.multiple_members(true);
    ReadBody::new(decoder)
        .map_err(|err| Box::new(err) as OutError)
        .boxed()
}

/// A body that hands its upstream connection back to the pool when it ends.
///
/// `net/http`'s transport returns a connection to its pool once the response
/// body is read out; a body that is still streaming holds it. The port's bodies
/// are not tied to the connection, so the tie is made explicitly here --
/// otherwise every streamed response would retire its connection and the next
/// request on the same tunnel would pay for a handshake.
struct Pooled {
    body: Out,
    recycle: Option<(Arc<Pool>, SendRequest<Out>)>,
}

impl Pooled {
    fn new(body: Out, pool: Arc<Pool>, send: SendRequest<Out>) -> Pooled {
        Pooled {
            body,
            recycle: Some((pool, send)),
        }
    }

    /// Give the connection back, at most once.
    fn recycle(&mut self) {
        if let Some((pool, send)) = self.recycle.take() {
            pool.put(send);
        }
    }
}

impl HttpBody for Pooled {
    type Data = Bytes;
    type Error = OutError;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, OutError>>> {
        let this = self.get_mut();
        match Pin::new(&mut this.body).poll_frame(cx) {
            Poll::Ready(Some(Ok(frame))) => Poll::Ready(Some(Ok(frame))),
            Poll::Ready(Some(Err(err))) => {
                this.recycle();
                Poll::Ready(Some(Err(err)))
            }
            Poll::Ready(None) => {
                this.recycle();
                Poll::Ready(None)
            }
            Poll::Pending => Poll::Pending,
        }
    }

    fn is_end_stream(&self) -> bool {
        self.body.is_end_stream()
    }

    fn size_hint(&self) -> SizeHint {
        self.body.size_hint()
    }
}

impl Drop for Pooled {
    fn drop(&mut self) {
        self.recycle();
    }
}

/// Wrap a streamed body so its connection is recycled when the body ends.
fn pooled(body: Out, pool: Arc<Pool>, send: SendRequest<Out>) -> Out {
    Pooled::new(body, pool, send).boxed()
}

/// Two bodies in sequence: the first, then the second. Its length is unknown,
/// which is the point -- it is how a prefix that was read out is put back.
struct Pair {
    first: Option<Out>,
    second: Out,
}

impl Pair {
    fn new(first: Out, second: Out) -> Pair {
        Pair {
            first: Some(first),
            second,
        }
    }
}

impl HttpBody for Pair {
    type Data = Bytes;
    type Error = OutError;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, OutError>>> {
        let this = self.get_mut();
        if let Some(first) = this.first.as_mut() {
            match Pin::new(first).poll_frame(cx) {
                Poll::Ready(Some(frame)) => return Poll::Ready(Some(frame)),
                Poll::Pending => return Poll::Pending,
                Poll::Ready(None) => this.first = None,
            }
        }
        Pin::new(&mut this.second).poll_frame(cx)
    }

    fn is_end_stream(&self) -> bool {
        self.first.is_none() && self.second.is_end_stream()
    }

    fn size_hint(&self) -> SizeHint {
        SizeHint::default()
    }
}

/// A body over anything that reads: what a decoder is, once it has to be a body.
struct ReadBody<R> {
    reader: R,
}

impl<R> ReadBody<R> {
    fn new(reader: R) -> ReadBody<R> {
        ReadBody { reader }
    }
}

impl<R: AsyncRead + Unpin> HttpBody for ReadBody<R> {
    type Data = Bytes;
    type Error = io::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, io::Error>>> {
        let this = self.get_mut();
        let mut chunk = [0u8; READ_CHUNK];
        let mut buffer = tokio::io::ReadBuf::new(&mut chunk);
        match Pin::new(&mut this.reader).poll_read(cx, &mut buffer) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(Err(err)) => Poll::Ready(Some(Err(err))),
            Poll::Ready(Ok(())) => {
                if buffer.filled().is_empty() {
                    Poll::Ready(None)
                } else {
                    Poll::Ready(Some(Ok(Frame::data(Bytes::copy_from_slice(
                        buffer.filled(),
                    )))))
                }
            }
        }
    }
}

/// The `Infallible` a `Full` body cannot produce.
fn never(error: std::convert::Infallible) -> OutError {
    match error {}
}

#[cfg(test)]
mod tests {
    use hyper::header::HeaderName;

    use super::*;

    fn headers(pairs: &[(&str, &str)]) -> HeaderMap {
        let mut head = HeaderMap::new();
        for (name, value) in pairs {
            head.append(
                HeaderName::from_bytes(name.as_bytes()).unwrap_or(HeaderName::from_static("x")),
                HeaderValue::from_str(value).unwrap_or(HeaderValue::from_static("")),
            );
        }
        head
    }

    /// Go's server moves the authority out of the header map and the framing
    /// header with it; hyper leaves both, so the port drops them. The row that
    /// catches getting this wrong compares every header the hook sees.
    #[test]
    fn the_authority_is_not_a_header() {
        let dropped = reported(&headers(&[
            ("host", "example.test"),
            ("transfer-encoding", "chunked"),
            ("content-length", "5"),
            ("accept-encoding", "identity"),
        ]));
        assert!(!dropped.contains_key(header::HOST));
        assert!(!dropped.contains_key(header::TRANSFER_ENCODING));
        assert_eq!(
            dropped
                .get(header::CONTENT_LENGTH)
                .and_then(|value| value.to_str().ok()),
            Some("5"),
            "core keeps content-length in the map, so the port must too"
        );
        assert_eq!(dropped.get_all(header::ACCEPT_ENCODING).iter().count(), 1);
    }

    #[test]
    fn framing_names_the_body_a_message_declared() {
        assert_eq!(
            framing_of(&headers(&[("content-length", "5")])),
            Framing::Length(5)
        );
        assert_eq!(
            framing_of(&headers(&[("transfer-encoding", "chunked")])),
            Framing::Chunked
        );
        assert_eq!(framing_of(&headers(&[])), Framing::None);
    }

    /// A refusal's shape is `http.Error`'s: the sentence, a newline, and the
    /// head that says so. Both rows that see one compare all four fields.
    #[test]
    fn a_refusal_is_core_s_sentence() {
        let response = refusal(StatusCode::FORBIDDEN, BLOCKED);
        assert_eq!(response.status(), StatusCode::FORBIDDEN);
        assert_eq!(
            response
                .headers()
                .get(header::CONTENT_LENGTH)
                .and_then(|value| value.to_str().ok()),
            Some("22")
        );
        assert_eq!(
            response
                .headers()
                .get(header::CONTENT_TYPE)
                .and_then(|value| value.to_str().ok()),
            Some("text/plain; charset=utf-8")
        );
        assert_eq!(
            response
                .headers()
                .get(header::X_CONTENT_TYPE_OPTIONS)
                .and_then(|value| value.to_str().ok()),
            Some("nosniff")
        );
    }
}
