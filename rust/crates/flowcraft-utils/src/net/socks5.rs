//! SOCKS5 (RFC 1928), both directions.
//!
//! The port speaks SOCKS5 twice: as a **server**, because a sandboxed client
//! that only knows `ALL_PROXY=socks5://...` has to reach the same rules an HTTP
//! client does, and as a **client**, because a proxy-mode upstream may itself
//! be a SOCKS5 proxy.
//!
//! # What is observable, and what is not
//!
//! A SOCKS5 session has one channel: the reply bytes. Core's session errors
//! (`socks5: bad version 0x04`, `socks5: zero destination port`) are never
//! written anywhere -- the session either writes a reply or writes nothing, and
//! then closes. So the contract for the server half is
//! `rust/conformance/wire-contract.json`'s `sessions` table: input bytes in,
//! reply bytes out, and the destination the audit record names. The error
//! values below exist for the caller's log line, not for the peer.
//!
//! The client half is different: its refusals *do* reach a client, wrapped in
//! the `502` body of whatever HTTP request needed the upstream. Those texts are
//! `golang.org/x/net/internal/socks`', ported verbatim, because a caller may be
//! matching on them.
//!
//! # Two splitters, and which one applies
//!
//! Core has two. [`super::proxy::split_host_port`] falls back when a split
//! fails; `net.SplitHostPort` inside the SOCKS5 client refuses instead. The
//! server reads its destination off the wire, so neither applies. The client is
//! handed a `host:port` by the proxy, so the strict one does, and
//! [`ClientError::MissingPort`] is what a malformed one becomes.
//!
//! # No half-close
//!
//! Core tunnels with `io.Copy` in both directions and closes the target as soon
//! as the client stops sending, so the first EOF tears the whole tunnel down.
//! [`pump`] keeps that, rather than doing the nicer thing: a hijacked Go
//! connection is not half-closed either, and a port that improved on it here
//! would answer differently from core on every request/response protocol that
//! relies on the response outliving the request body.

use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::time::Duration;

use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, BufReader};

use super::BoxFuture;
use super::proxy::join_host_port;

/// The protocol version byte. Also the mux's classifier: a connection whose
/// first byte is this one is a SOCKS5 session.
pub const VERSION: u8 = 0x05;
/// "No authentication required" (RFC 1928 §3).
pub const NO_AUTH: u8 = 0x00;
/// "Username/password" (RFC 1929).
pub const USER_PASS: u8 = 0x02;
/// "No acceptable methods" -- the reply that ends a greeting.
pub const NO_ACCEPTABLE: u8 = 0xff;
/// The only command the proxy implements.
pub const CMD_CONNECT: u8 = 0x01;
/// Address types (RFC 1928 §4).
pub const ATYP_IPV4: u8 = 0x01;
/// A domain name, length-prefixed.
pub const ATYP_DOMAIN: u8 = 0x03;
/// A 16-byte IPv6 address.
pub const ATYP_IPV6: u8 = 0x04;
/// The request succeeded.
pub const SUCCESS: u8 = 0x00;
/// The request failed, for a reason this protocol has no code for.
pub const FAILURE: u8 = 0x01;
/// The command is not implemented (BIND, UDP ASSOCIATE).
pub const CMD_UNSUPPORTED: u8 = 0x07;

/// Core's handshake deadline: the greeting and the request share it, and the
/// tunnel that follows is unbounded.
pub const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(15);
/// Core's dial deadline: a client that disconnects while a black-holed target
/// is being dialed must not pin the session for the system's TCP timeout.
pub const DIAL_TIMEOUT: Duration = Duration::from_secs(30);

/// One CONNECT destination, as it came off the wire.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    /// The host, without brackets: `example.com`, `203.0.113.7`, `2001:db8::1`.
    pub host: String,
    /// The destination port. Never zero: the server refuses that.
    pub port: u16,
}

impl Target {
    /// `net.JoinHostPort`: brackets around anything with a colon in it.
    pub fn host_port(&self) -> String {
        join_host_port(&self.host, i32::from(self.port))
    }
}

/// A connection to a target, and the local address it got.
///
/// The local address is what the success reply reports as `BND.ADDR`, and core
/// reports it only when the dial produced a TCP socket -- a unix-socket target
/// has nothing to report, which is why this is an `Option`.
pub struct Dialed {
    /// The established connection.
    pub stream: Box<dyn Stream>,
    /// The local address of the connection, when it has one.
    pub local: Option<SocketAddr>,
}

/// What a session needs from a connection: both directions, and nothing else.
pub trait Stream: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send + ?Sized> Stream for T {}

/// Dials a destination the policy already allowed.
///
/// It is a trait because the session has no business knowing whether the dial
/// goes direct, through an HTTP `CONNECT` upstream or through a SOCKS5 one --
/// and because a test can then put an in-memory stream behind it.
pub trait Connector: Send + Sync {
    /// Dial one target. The error is reported to the client as a generic
    /// failure reply; it is not part of the protocol.
    fn connect<'a>(&'a self, target: &'a Target) -> BoxFuture<'a, io::Result<Dialed>>;
}

impl<T: Connector + ?Sized> Connector for std::sync::Arc<T> {
    fn connect<'a>(&'a self, target: &'a Target) -> BoxFuture<'a, io::Result<Dialed>> {
        (**self).connect(target)
    }
}

/// Why a server session ended before it tunnelled.
///
/// Nothing here reaches the peer: the reply (or its absence) is the whole of
/// what a client observes. The text exists so a caller can log a dropped
/// connection, and it is core's text.
#[derive(Debug)]
pub enum Error {
    /// The stream failed. Core lets these through untouched.
    Io(io::Error),
    /// The greeting's version byte was not `0x05`.
    BadVersion(u8),
    /// The client offered no method the server implements.
    NoAcceptableMethod,
    /// The request's version byte was not `0x05`.
    BadRequestVersion(u8),
    /// The command was not CONNECT (BIND, UDP ASSOCIATE, or nonsense).
    UnsupportedCommand(u8),
    /// The address type was none of IPv4, domain, IPv6.
    UnsupportedAddressType(u8),
    /// A domain name of length zero.
    EmptyDomain,
    /// A destination port of zero.
    ZeroPort,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Io(err) => write!(f, "socks5: {err}"),
            Error::BadVersion(version) => write!(f, "socks5: bad version 0x{version:02x}"),
            Error::NoAcceptableMethod => f.write_str("socks5: no acceptable auth method"),
            Error::BadRequestVersion(version) => {
                write!(f, "socks5: bad request version 0x{version:02x}")
            }
            Error::UnsupportedCommand(command) => {
                write!(f, "socks5: unsupported command 0x{command:02x}")
            }
            Error::UnsupportedAddressType(kind) => {
                write!(f, "socks5: unsupported address type 0x{kind:02x}")
            }
            Error::EmptyDomain => f.write_str("socks5: empty domain"),
            Error::ZeroPort => f.write_str("socks5: zero destination port"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Io(err) => Some(err),
            _ => None,
        }
    }
}

impl From<io::Error> for Error {
    fn from(err: io::Error) -> Error {
        Error::Io(err)
    }
}

/// A SOCKS5 server session.
///
/// One instance serves any number of connections: it holds the connector and
/// the two deadlines, and nothing per-connection.
pub struct Server<C: Connector> {
    connect: C,
    handshake_timeout: Duration,
    dial_timeout: Duration,
}

impl<C: Connector> Server<C> {
    /// A server that dials through `connect`, on core's deadlines.
    pub fn new(connect: C) -> Server<C> {
        Server {
            connect,
            handshake_timeout: HANDSHAKE_TIMEOUT,
            dial_timeout: DIAL_TIMEOUT,
        }
    }

    /// The same server with other deadlines (core keeps these as variables so
    /// its tests can shrink them).
    pub fn with_timeouts(
        mut self,
        handshake: Duration,
        dial: Duration,
    ) -> Server<C> {
        self.handshake_timeout = handshake;
        self.dial_timeout = dial;
        self
    }

    /// Serve one connection whose first byte is already known to be `0x05`.
    ///
    /// `client` is a [`BufReader`] because the mux that classified the
    /// connection read its first byte through one: the buffer may hold the rest
    /// of the request, and a client that pipelines the request with its first
    /// payload would otherwise lose those bytes.
    ///
    /// The connection is dropped when this returns -- that is core's `defer
    /// conn.Close()`, and the only way a client learns a session ended early.
    pub async fn serve<S>(&self, client: BufReader<S>)
    where
        S: AsyncRead + AsyncWrite + Unpin + Send,
    {
        let (mut client_read, mut client_write) = tokio::io::split(client);
        let handshake = async {
            greeting(&mut client_read, &mut client_write).await?;
            request(&mut client_read, &mut client_write).await
        };
        let target = match tokio::time::timeout(self.handshake_timeout, handshake).await {
            Ok(Ok(target)) => target,
            Ok(Err(_)) | Err(_) => return,
        };

        let dialed = match tokio::time::timeout(self.dial_timeout, self.connect.connect(&target))
            .await
        {
            Ok(Ok(dialed)) => dialed,
            Ok(Err(_)) | Err(_) => {
                let _ = write_reply(&mut client_write, FAILURE, None).await;
                return;
            }
        };
        if write_reply(&mut client_write, SUCCESS, dialed.local)
            .await
            .is_err()
        {
            return;
        }
        let (mut target_read, mut target_write) = tokio::io::split(dialed.stream);
        pump(&mut client_read, &mut client_write, &mut target_read, &mut target_write).await;
    }
}

/// Copy both ways until either direction ends, then stop.
///
/// The halves are passed in rather than the streams so that the caller can keep
/// whatever it still needs; the teardown is what matters, not the ownership.
pub async fn pump<R1, W1, R2, W2>(
    client_read: &mut R1,
    client_write: &mut W1,
    target_read: &mut R2,
    target_write: &mut W2,
) where
    R1: AsyncRead + Unpin,
    W1: AsyncWrite + Unpin,
    R2: AsyncRead + Unpin,
    W2: AsyncWrite + Unpin,
{
    tokio::select! {
        // The client stopped sending: core closes the target right there,
        // which is what ends the other direction too.
        _ = tokio::io::copy(client_read, target_write) => {
            let _ = target_write.shutdown().await;
        }
        // The target ended: the response is complete, so the client's side is
        // flushed and the tunnel is over.
        _ = tokio::io::copy(target_read, client_write) => {
            let _ = client_write.flush().await;
        }
    }
}

/// Read the greeting and answer it.
///
/// On `NoAcceptableMethod` the `0x05 0xff` reply has already gone out, which is
/// the only thing distinguishing it from a dropped connection.
async fn greeting<R, W>(reader: &mut R, writer: &mut W) -> Result<u8, Error>
where
    R: AsyncRead + Unpin,
    W: AsyncWrite + Unpin,
{
    let mut version = [0u8; 1];
    reader.read_exact(&mut version).await?;
    if version[0] != VERSION {
        return Err(Error::BadVersion(version[0]));
    }
    let mut count = [0u8; 1];
    reader.read_exact(&mut count).await?;
    let mut methods = vec![0u8; usize::from(count[0])];
    reader.read_exact(&mut methods).await?;
    // Core offers to pick no-auth the moment it sees one, wherever it sits in
    // the list, and refuses outright when it does not.
    if methods.contains(&NO_AUTH) {
        writer.write_all(&[VERSION, NO_AUTH]).await?;
        Ok(NO_AUTH)
    } else {
        writer.write_all(&[VERSION, NO_ACCEPTABLE]).await?;
        Err(Error::NoAcceptableMethod)
    }
}

/// Parse the CONNECT request.
///
/// A non-CONNECT command gets its `0x07` reply before the refusal, because the
/// client is entitled to know the command was understood and not implemented.
/// Every other refusal is silent.
async fn request<R, W>(reader: &mut R, writer: &mut W) -> Result<Target, Error>
where
    R: AsyncRead + Unpin,
    W: AsyncWrite + Unpin,
{
    let mut head = [0u8; 3];
    reader.read_exact(&mut head).await?;
    if head[0] != VERSION {
        return Err(Error::BadRequestVersion(head[0]));
    }
    if head[1] != CMD_CONNECT {
        let _ = write_reply(writer, CMD_UNSUPPORTED, None).await;
        return Err(Error::UnsupportedCommand(head[1]));
    }
    let mut atyp = [0u8; 1];
    reader.read_exact(&mut atyp).await?;
    let host = match atyp[0] {
        ATYP_IPV4 => {
            let mut address = [0u8; 4];
            reader.read_exact(&mut address).await?;
            Ipv4Addr::from(address).to_string()
        }
        ATYP_IPV6 => {
            let mut address = [0u8; 16];
            reader.read_exact(&mut address).await?;
            Ipv6Addr::from(address).to_string()
        }
        ATYP_DOMAIN => {
            let mut length = [0u8; 1];
            reader.read_exact(&mut length).await?;
            if length[0] == 0 {
                return Err(Error::EmptyDomain);
            }
            let mut name = vec![0u8; usize::from(length[0])];
            reader.read_exact(&mut name).await?;
            // A hostname is UTF-8 by definition; a name that is not cannot be
            // dialed. Core keeps the bytes and lets the dial fail, and so does
            // this: lossy, so the failure is still a dial failure.
            String::from_utf8_lossy(&name).into_owned()
        }
        other => return Err(Error::UnsupportedAddressType(other)),
    };
    let mut port = [0u8; 2];
    reader.read_exact(&mut port).await?;
    let port = u16::from_be_bytes(port);
    if port == 0 {
        return Err(Error::ZeroPort);
    }
    Ok(Target { host, port })
}

/// Write one reply, reporting `local` as the bound address when there is one.
async fn write_reply<W>(writer: &mut W, code: u8, local: Option<SocketAddr>) -> io::Result<()>
where
    W: AsyncWrite + Unpin,
{
    writer.write_all(&encode_reply(code, local)).await
}

/// The reply's wire form: `VER, REP, RSV, ATYP, BND.ADDR, BND.PORT`.
pub fn encode_reply(code: u8, local: Option<SocketAddr>) -> Vec<u8> {
    let mut out = vec![VERSION, code, 0x00];
    match local {
        Some(SocketAddr::V4(address)) => {
            out.push(ATYP_IPV4);
            out.extend_from_slice(&address.ip().octets());
            out.extend_from_slice(&address.port().to_be_bytes());
        }
        Some(SocketAddr::V6(address)) => {
            out.push(ATYP_IPV6);
            out.extend_from_slice(&address.ip().octets());
            out.extend_from_slice(&address.port().to_be_bytes());
        }
        // Clients ignore BND.ADDR for CONNECT, which is why core sends zeros
        // when the dial produced something that is not a TCP socket.
        None => {
            out.push(ATYP_IPV4);
            out.extend_from_slice(&[0, 0, 0, 0]);
            out.extend_from_slice(&0u16.to_be_bytes());
        }
    }
    out
}

/// Credentials for the username/password method (RFC 1929).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Credentials {
    /// The username. One to 255 bytes, or the method cannot be used.
    pub username: String,
    /// The password. Up to 255 bytes, and possibly empty.
    pub password: String,
}

/// Why a client-side negotiation failed.
///
/// Every variant is a message the caller may end up showing: these errors travel
/// through the upstream dial into a `502` body, so the texts are the reference
/// client's (`golang.org/x/net/internal/socks`), not this module's.
#[derive(Debug)]
pub enum ClientError {
    /// The address had no `host:port` split at all.
    MissingPort(String),
    /// The address's port was not a number.
    InvalidPort(String),
    /// The port was a number outside `1..=65535`.
    PortOutOfRange(String),
    /// The upstream answered with a version byte other than `0x05`.
    UnexpectedVersion(u8),
    /// The upstream accepted none of the offered methods.
    NoAcceptableMethods,
    /// The credentials could not be sent: no username, or a field over 255.
    InvalidCredentials,
    /// The RFC 1929 sub-negotiation answered with another version byte.
    BadAuthVersion,
    /// The upstream rejected the credentials.
    AuthFailed,
    /// A domain name longer than 255 bytes.
    FqdnTooLong,
    /// The reply's address type was none of IPv4, domain, IPv6.
    UnknownAddressType(u8),
    /// The reply's reserved field was not zero.
    NonZeroReserved,
    /// The reply carried a status other than "succeeded".
    Status(u8),
    /// The stream failed.
    Io(io::Error),
}

impl std::fmt::Display for ClientError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ClientError::MissingPort(address) => {
                write!(f, "address {address}: missing port in address")
            }
            ClientError::InvalidPort(address) => write!(f, "address {address}: invalid port"),
            ClientError::PortOutOfRange(port) => write!(f, "port number out of range {port}"),
            ClientError::UnexpectedVersion(version) => {
                write!(f, "unexpected protocol version {version}")
            }
            ClientError::NoAcceptableMethods => {
                f.write_str("no acceptable authentication methods")
            }
            ClientError::InvalidCredentials => f.write_str("invalid username/password"),
            ClientError::BadAuthVersion => f.write_str("invalid username/password version"),
            ClientError::AuthFailed => f.write_str("username/password authentication failed"),
            ClientError::FqdnTooLong => f.write_str("FQDN too long"),
            ClientError::UnknownAddressType(kind) => write!(f, "unknown address type {kind}"),
            ClientError::NonZeroReserved => f.write_str("non-zero reserved field"),
            ClientError::Status(status) => write!(f, "unknown error {}", status_text(*status)),
            ClientError::Io(err) => write!(f, "{err}"),
        }
    }
}

impl std::error::Error for ClientError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            ClientError::Io(err) => Some(err),
            _ => None,
        }
    }
}

impl From<io::Error> for ClientError {
    fn from(err: io::Error) -> ClientError {
        ClientError::Io(err)
    }
}

/// `Reply.String()`: the status names the reference client prints.
pub fn status_text(status: u8) -> String {
    match status {
        0x00 => "succeeded".to_owned(),
        0x01 => "general SOCKS server failure".to_owned(),
        0x02 => "connection not allowed by ruleset".to_owned(),
        0x03 => "network unreachable".to_owned(),
        0x04 => "host unreachable".to_owned(),
        0x05 => "connection refused".to_owned(),
        0x06 => "TTL expired".to_owned(),
        0x07 => "command not supported".to_owned(),
        0x08 => "address type not supported".to_owned(),
        other => format!("unknown code: {other}"),
    }
}

/// Negotiate a CONNECT through a SOCKS5 proxy, on an already-connected stream.
///
/// This is `socks.Dialer.connect` without the dial: the caller owns the
/// connection to the proxy, so a test can hand it a pipe.
pub async fn negotiate<S>(
    stream: &mut S,
    address: &str,
    credentials: Option<&Credentials>,
) -> Result<(), ClientError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    let (host, port) = split_strict(address)?;
    // With credentials both methods are offered, no-auth first: the upstream
    // picks, and a proxy that needs no authentication takes the first.
    let greeting = match credentials {
        Some(_) => vec![VERSION, 2, NO_AUTH, USER_PASS],
        None => vec![VERSION, 1, NO_AUTH],
    };
    stream.write_all(&greeting).await?;
    let mut method = [0u8; 2];
    stream.read_exact(&mut method).await?;
    if method[0] != VERSION {
        return Err(ClientError::UnexpectedVersion(method[0]));
    }
    if method[1] == NO_ACCEPTABLE {
        return Err(ClientError::NoAcceptableMethods);
    }
    if let Some(credentials) = credentials {
        authenticate(stream, credentials, method[1]).await?;
    }

    let mut request = vec![VERSION, CMD_CONNECT, 0x00];
    match host.parse::<IpAddr>() {
        Ok(address) => push_address(&mut request, address)?,
        Err(_) => {
            if host.len() > 255 {
                return Err(ClientError::FqdnTooLong);
            }
            push_domain(&mut request, &host);
        }
    }
    request.extend_from_slice(&port.to_be_bytes());
    stream.write_all(&request).await?;

    let mut head = [0u8; 4];
    stream.read_exact(&mut head).await?;
    if head[0] != VERSION {
        return Err(ClientError::UnexpectedVersion(head[0]));
    }
    if head[1] != SUCCESS {
        return Err(ClientError::Status(head[1]));
    }
    if head[2] != 0 {
        return Err(ClientError::NonZeroReserved);
    }
    // The bound address is read to keep the stream positioned, never used.
    match head[3] {
        ATYP_IPV4 => {
            let mut skip = [0u8; 6];
            stream.read_exact(&mut skip).await?;
        }
        ATYP_IPV6 => {
            let mut skip = [0u8; 18];
            stream.read_exact(&mut skip).await?;
        }
        ATYP_DOMAIN => {
            let mut length = [0u8; 1];
            stream.read_exact(&mut length).await?;
            let mut skip = vec![0u8; usize::from(length[0]) + 2];
            stream.read_exact(&mut skip).await?;
        }
        other => return Err(ClientError::UnknownAddressType(other)),
    }
    Ok(())
}

/// RFC 1929: version, length-prefixed username, length-prefixed password.
async fn authenticate<S>(
    stream: &mut S,
    credentials: &Credentials,
    method: u8,
) -> Result<(), ClientError>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    match method {
        NO_AUTH => Ok(()),
        USER_PASS => {
            if credentials.username.is_empty()
                || credentials.username.len() > 255
                || credentials.password.len() > 255
            {
                return Err(ClientError::InvalidCredentials);
            }
            let mut request = vec![0x01, credentials.username.len() as u8];
            request.extend_from_slice(credentials.username.as_bytes());
            request.push(credentials.password.len() as u8);
            request.extend_from_slice(credentials.password.as_bytes());
            stream.write_all(&request).await?;
            let mut reply = [0u8; 2];
            stream.read_exact(&mut reply).await?;
            if reply[0] != 0x01 {
                return Err(ClientError::BadAuthVersion);
            }
            if reply[1] != 0x00 {
                return Err(ClientError::AuthFailed);
            }
            Ok(())
        }
        other => {
            // The reference client refuses a method it did not offer.
            let _ = other;
            Err(ClientError::NoAcceptableMethods)
        }
    }
}

fn push_address(request: &mut Vec<u8>, address: IpAddr) -> Result<(), ClientError> {
    match address {
        IpAddr::V4(v4) => {
            request.push(ATYP_IPV4);
            request.extend_from_slice(&v4.octets());
        }
        IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            // `net.IP.To4` folds a v4-mapped address, so the reference client
            // asks for it as a v4 address.
            Some(v4) => {
                request.push(ATYP_IPV4);
                request.extend_from_slice(&v4.octets());
            }
            None => {
                request.push(ATYP_IPV6);
                request.extend_from_slice(&v6.octets());
            }
        },
    }
    Ok(())
}

fn push_domain(request: &mut Vec<u8>, host: &str) {
    request.push(ATYP_DOMAIN);
    request.push(host.len() as u8);
    request.extend_from_slice(host.as_bytes());
}

/// `net.SplitHostPort` plus `strconv.Atoi` and the range check the reference
/// client makes: the three ways an address is refused before anything is
/// written.
fn split_strict(address: &str) -> Result<(String, u16), ClientError> {
    let (host, port) = match super::proxy::split_host_port_strict(address) {
        Some(split) => split,
        None => return Err(ClientError::MissingPort(address.to_owned())),
    };
    let number = match port.parse::<i64>() {
        Ok(number) => number,
        Err(_) => return Err(ClientError::InvalidPort(address.to_owned())),
    };
    if !(1..=0xffff).contains(&number) {
        return Err(ClientError::PortOutOfRange(port.to_owned()));
    }
    Ok((host.to_owned(), number as u16))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    /// A connector that echoes whatever is written to it, and records the
    /// destinations it was asked for.
    struct Echo {
        asked: Arc<Mutex<Vec<String>>>,
        local: Option<SocketAddr>,
        fails: bool,
    }

    impl Echo {
        fn new() -> Echo {
            Echo {
                asked: Arc::new(Mutex::new(Vec::new())),
                local: Some(SocketAddr::from(([127, 0, 0, 1], 8080))),
                fails: false,
            }
        }
    }

    impl Connector for Echo {
        fn connect<'a>(&'a self, target: &'a Target) -> BoxFuture<'a, io::Result<Dialed>> {
            if let Ok(mut asked) = self.asked.lock() {
                asked.push(target.host_port());
            }
            let local = self.local;
            let fails = self.fails;
            Box::pin(async move {
                if fails {
                    return Err(io::Error::other("no route"));
                }
                let (service, client) = tokio::io::duplex(64 * 1024);
                tokio::spawn(async move {
                    let (mut read, mut write) = tokio::io::split(client);
                    let _ = tokio::io::copy(&mut read, &mut write).await;
                });
                Ok(Dialed {
                    stream: Box::new(service),
                    local,
                })
            })
        }
    }

    fn ok<T, E: std::fmt::Debug>(result: Result<T, E>, what: &str) -> T {
        match result {
            Ok(value) => value,
            Err(err) => panic!("{what}: {err:?}"),
        }
    }

    /// Feed `input` to a fresh session and collect everything it writes back.
    ///
    /// The collection stops when the session closes or when it goes quiet: a
    /// tunnelled session stays open until one of its ends does, and core's
    /// teardown (the first EOF ends both directions) means the write side must
    /// stay open for the response to arrive.
    async fn session<C: Connector + 'static>(server: Server<C>, input: &[u8]) -> Vec<u8> {
        let (mut client, service) = tokio::io::duplex(64 * 1024);
        let task = tokio::spawn(async move {
            server.serve(BufReader::new(service)).await;
        });
        ok(client.write_all(input).await, "write input");
        let mut output = Vec::new();
        let mut buffer = [0u8; 4096];
        loop {
            match tokio::time::timeout(Duration::from_millis(100), client.read(&mut buffer)).await
            {
                Ok(Ok(0)) | Ok(Err(_)) | Err(_) => break,
                Ok(Ok(read)) => output.extend_from_slice(&buffer[..read]),
            }
        }
        drop(client);
        let _ = task.await;
        output
    }

    fn greeting_no_auth(host: &str, port: u16) -> Vec<u8> {
        let mut bytes = vec![VERSION, 1, NO_AUTH, VERSION, CMD_CONNECT, 0x00];
        match host.parse::<IpAddr>() {
            Ok(IpAddr::V4(v4)) => {
                bytes.push(ATYP_IPV4);
                bytes.extend_from_slice(&v4.octets());
            }
            Ok(IpAddr::V6(v6)) => {
                bytes.push(ATYP_IPV6);
                bytes.extend_from_slice(&v6.octets());
            }
            Err(_) => {
                bytes.push(ATYP_DOMAIN);
                bytes.push(host.len() as u8);
                bytes.extend_from_slice(host.as_bytes());
            }
        }
        bytes.extend_from_slice(&port.to_be_bytes());
        bytes
    }

    /// The two replies an allowed domain session produces, minus the bind
    /// address this connector invents.
    #[tokio::test]
    async fn a_domain_tunnel_answers_and_echoes() {
        let mut input = greeting_no_auth("example.com", 443);
        input.extend_from_slice(b"ping");
        let output = session(Server::new(Echo::new()), &input).await;
        let mut expected = vec![VERSION, NO_AUTH];
        expected.extend_from_slice(&encode_reply(
            SUCCESS,
            Some(SocketAddr::from(([127, 0, 0, 1], 8080))),
        ));
        expected.extend_from_slice(b"ping");
        assert_eq!(output, expected);
    }

    #[tokio::test]
    async fn the_parsed_target_is_the_one_dialed() {
        for (host, port) in [
            ("example.com", 443u16),
            ("203.0.113.7", 80),
            ("2001:db8::1", 8443),
        ] {
            let connector = Arc::new(Echo::new());
            let asked = Arc::clone(&connector.asked);
            let output = session(
                Server::new(connector),
                &greeting_no_auth(host, port),
            )
            .await;
            assert!(
                output.len() >= 2 && output[1] == SUCCESS,
                "{host} was refused: {output:?}"
            );
            let asked = asked.lock().map(|asked| asked.clone()).unwrap_or_default();
            let wanted = Target {
                host: host.to_owned(),
                port,
            };
            assert_eq!(asked, vec![wanted.host_port()], "{host}");
        }
    }

    #[tokio::test]
    async fn a_denied_destination_gets_the_generic_failure_reply() {
        let mut echo = Echo::new();
        echo.fails = true;
        let output = session(Server::new(echo), &greeting_no_auth("blocked.example", 443)).await;
        let mut expected = vec![VERSION, NO_AUTH];
        expected.extend_from_slice(&encode_reply(FAILURE, None));
        assert_eq!(output, expected);
    }

    #[tokio::test]
    async fn a_bad_version_is_dropped_without_a_word() {
        assert_eq!(
            session(Server::new(Echo::new()), &[0x04, 0x01, 0x00]).await,
            Vec::<u8>::new()
        );
    }

    #[tokio::test]
    async fn no_acceptable_method_is_answered_then_dropped() {
        // The client offers username/password only, which this server does not
        // implement.
        assert_eq!(
            session(Server::new(Echo::new()), &[VERSION, 0x01, USER_PASS]).await,
            vec![VERSION, NO_ACCEPTABLE]
        );
        // An empty method list is the same answer.
        assert_eq!(
            session(Server::new(Echo::new()), &[VERSION, 0x00]).await,
            vec![VERSION, NO_ACCEPTABLE]
        );
    }

    #[tokio::test]
    async fn no_auth_is_found_wherever_it_sits() {
        let output = session(
            Server::new(Echo::new()),
            &[VERSION, 0x02, USER_PASS, NO_AUTH, VERSION, CMD_CONNECT, 0x00, ATYP_IPV4, 127, 0, 0, 1, 0, 80],
        )
        .await;
        assert_eq!(output.first().copied(), Some(VERSION));
        assert_eq!(output.get(1).copied(), Some(NO_AUTH));
    }

    #[tokio::test]
    async fn a_command_the_proxy_does_not_implement_is_named() {
        // UDP ASSOCIATE: the client is told the command was understood and not
        // implemented, which is the one refusal that is not silence.
        let output = session(
            Server::new(Echo::new()),
            &[VERSION, 0x01, NO_AUTH, VERSION, 0x03, 0x00, ATYP_IPV4, 127, 0, 0, 1, 0, 53],
        )
        .await;
        let mut expected = vec![VERSION, NO_AUTH];
        expected.extend_from_slice(&encode_reply(CMD_UNSUPPORTED, None));
        assert_eq!(output, expected);
    }

    #[tokio::test]
    async fn a_malformed_request_is_dropped_silently() {
        let opened = [VERSION, 0x01, NO_AUTH][..].to_vec();
        let mut cases: Vec<(&str, Vec<u8>)> = Vec::new();
        // An address type nobody implements.
        cases.push(("atyp", {
            let mut bytes = opened.clone();
            bytes.extend_from_slice(&[VERSION, CMD_CONNECT, 0x00, 0x02, 127, 0, 0, 1, 0, 80]);
            bytes
        }));
        // A domain of length zero.
        cases.push(("empty-domain", {
            let mut bytes = opened.clone();
            bytes.extend_from_slice(&[VERSION, CMD_CONNECT, 0x00, ATYP_DOMAIN, 0x00, 0, 80]);
            bytes
        }));
        // A destination port of zero.
        cases.push(("zero-port", {
            let mut bytes = opened.clone();
            bytes.extend_from_slice(&[VERSION, CMD_CONNECT, 0x00, ATYP_IPV4, 127, 0, 0, 1, 0, 0]);
            bytes
        }));
        // A request that stops in the middle of the address.
        cases.push(("truncated", {
            let mut bytes = opened.clone();
            bytes.extend_from_slice(&[VERSION, CMD_CONNECT, 0x00, ATYP_IPV4, 127, 0]);
            bytes
        }));
        for (name, input) in cases {
            assert_eq!(
                session(Server::new(Echo::new()), &input).await,
                vec![VERSION, NO_AUTH],
                "{name}"
            );
        }
    }

    #[tokio::test]
    async fn the_handshake_is_bounded() {
        let server = Server::new(Echo::new())
            .with_timeouts(Duration::from_millis(50), Duration::from_secs(5));
        // A client that connects and says nothing must not pin the session.
        assert_eq!(session(server, &[VERSION]).await, Vec::<u8>::new());
    }

    #[test]
    fn a_reply_reports_the_bind_address_it_was_given() {
        assert_eq!(
            encode_reply(SUCCESS, Some(SocketAddr::from(([127, 0, 0, 1], 8080)))),
            vec![VERSION, SUCCESS, 0x00, ATYP_IPV4, 127, 0, 0, 1, 0x1f, 0x90]
        );
        let address = match "2001:db8::1".parse::<IpAddr>() {
            Ok(IpAddr::V6(v6)) => v6,
            _ => Ipv6Addr::LOCALHOST,
        };
        let mut expected = vec![VERSION, SUCCESS, 0x00, ATYP_IPV6];
        expected.extend_from_slice(&address.octets());
        expected.extend_from_slice(&443u16.to_be_bytes());
        assert_eq!(
            encode_reply(SUCCESS, Some(SocketAddr::new(IpAddr::V6(address), 443))),
            expected
        );
        // No socket, no address: clients ignore BND.ADDR for CONNECT.
        assert_eq!(
            encode_reply(FAILURE, None),
            vec![VERSION, FAILURE, 0x00, ATYP_IPV4, 0, 0, 0, 0, 0, 0]
        );
    }

    /// A scripted SOCKS5 proxy: reads what the client sends, answers with the
    /// bytes the test chose, and hands back everything it received.
    struct Script {
        greeting_reply: Vec<u8>,
        auth_reply: Vec<u8>,
        final_reply: Vec<u8>,
    }

    async fn negotiate_against(
        script: Script,
        address: &str,
        credentials: Option<&Credentials>,
    ) -> (Result<(), ClientError>, Vec<u8>) {
        let (mut client, mut service) = tokio::io::duplex(64 * 1024);
        // A client that refuses the greeting walks away without a request, so
        // every read here has to treat EOF as the end of the script.
        let server = tokio::spawn(async move {
            let mut received = Vec::new();
            let mut head = [0u8; 2];
            if service.read_exact(&mut head).await.is_err() {
                return received;
            }
            received.extend_from_slice(&head);
            let mut methods = vec![0u8; usize::from(head[1])];
            if service.read_exact(&mut methods).await.is_err() {
                return received;
            }
            received.extend_from_slice(&methods);
            ok(
                service.write_all(&script.greeting_reply).await,
                "greeting reply",
            );
            if script.greeting_reply == [VERSION, USER_PASS] {
                let mut auth = [0u8; 2];
                if service.read_exact(&mut auth).await.is_err() {
                    return received;
                }
                received.extend_from_slice(&auth);
                let mut username = vec![0u8; usize::from(auth[1])];
                if service.read_exact(&mut username).await.is_err() {
                    return received;
                }
                received.extend_from_slice(&username);
                let mut length = [0u8; 1];
                if service.read_exact(&mut length).await.is_err() {
                    return received;
                }
                received.push(length[0]);
                let mut password = vec![0u8; usize::from(length[0])];
                if service.read_exact(&mut password).await.is_err() {
                    return received;
                }
                received.extend_from_slice(&password);
                ok(service.write_all(&script.auth_reply).await, "auth reply");
            }
            let mut request = [0u8; 4];
            if service.read_exact(&mut request).await.is_err() {
                return received;
            }
            received.extend_from_slice(&request);
            let address_length = match request[3] {
                ATYP_IPV4 => 4,
                ATYP_IPV6 => 16,
                _ => {
                    let mut length = [0u8; 1];
                    if service.read_exact(&mut length).await.is_err() {
                        return received;
                    }
                    received.push(length[0]);
                    usize::from(length[0])
                }
            };
            let mut address = vec![0u8; address_length];
            if service.read_exact(&mut address).await.is_err() {
                return received;
            }
            received.extend_from_slice(&address);
            let mut port = [0u8; 2];
            if service.read_exact(&mut port).await.is_err() {
                return received;
            }
            received.extend_from_slice(&port);
            ok(service.write_all(&script.final_reply).await, "final reply");
            received
        });
        let result = negotiate(&mut client, address, credentials).await;
        // The client has to go before the script is awaited: a script that is
        // still waiting for a request needs the EOF a dropped client sends.
        drop(client);
        let received = ok(server.await, "scripted proxy");
        (result, received)
    }

    fn succeeded() -> Vec<u8> {
        encode_reply(SUCCESS, Some(SocketAddr::from(([0, 0, 0, 0], 0))))
    }

    #[tokio::test]
    async fn a_client_greets_with_the_methods_the_reference_client_offers() {
        let (result, received) = negotiate_against(
            Script {
                greeting_reply: vec![VERSION, NO_AUTH],
                auth_reply: Vec::new(),
                final_reply: succeeded(),
            },
            "example.com:443",
            None,
        )
        .await;
        assert_eq!(result.as_ref().err().map(ToString::to_string), None);
        let mut expected = vec![VERSION, 1, NO_AUTH, VERSION, CMD_CONNECT, 0x00, ATYP_DOMAIN];
        expected.push(11);
        expected.extend_from_slice(b"example.com");
        expected.extend_from_slice(&443u16.to_be_bytes());
        assert_eq!(received, expected);
    }

    #[tokio::test]
    async fn credentials_are_offered_and_sent_as_rfc_1929() {
        let credentials = Credentials {
            username: "user".to_owned(),
            password: "pass".to_owned(),
        };
        let (result, received) = negotiate_against(
            Script {
                greeting_reply: vec![VERSION, USER_PASS],
                auth_reply: vec![0x01, 0x00],
                final_reply: succeeded(),
            },
            "203.0.113.7:80",
            Some(&credentials),
        )
        .await;
        assert_eq!(result.as_ref().err().map(ToString::to_string), None);
        let expected = vec![
            VERSION, 2, NO_AUTH, USER_PASS, 0x01, 4, b'u', b's', b'e', b'r', 4, b'p', b'a', b's', b's',
            VERSION, CMD_CONNECT, 0x00, ATYP_IPV4, 203, 0, 113, 7, 0, 80,
        ];
        assert_eq!(received, expected);
    }

    #[tokio::test]
    async fn an_upstream_refusal_is_reported_the_way_the_reference_client_words_it() {
        let refused_status = encode_reply(0x05, None);
        let reserved = vec![VERSION, SUCCESS, 0x01, ATYP_IPV4, 0, 0, 0, 0, 0, 0];
        let cases: Vec<(&str, Vec<u8>, Vec<u8>, &str)> = vec![
            (
                "no-methods",
                vec![VERSION, NO_ACCEPTABLE],
                Vec::new(),
                "no acceptable authentication methods",
            ),
            (
                "version",
                vec![0x04, NO_AUTH],
                Vec::new(),
                "unexpected protocol version 4",
            ),
            (
                "status",
                vec![VERSION, NO_AUTH],
                refused_status,
                "unknown error connection refused",
            ),
            (
                "reserved",
                vec![VERSION, NO_AUTH],
                reserved,
                "non-zero reserved field",
            ),
        ];
        for (name, greeting_reply, final_reply, wanted) in cases {
            let (result, _) = negotiate_against(
                Script {
                    greeting_reply,
                    auth_reply: Vec::new(),
                    final_reply,
                },
                "example.com:443",
                None,
            )
            .await;
            let error = match result {
                Ok(()) => panic!("{name}: the negotiation should have failed"),
                Err(error) => error,
            };
            assert_eq!(error.to_string(), wanted, "{name}");
        }
    }

    #[tokio::test]
    async fn a_destination_without_a_port_is_refused_before_anything_is_written() {
        for (address, wanted) in [
            ("example.com", "address example.com: missing port in address"),
            ("example.com:", "address example.com:: invalid port"),
            ("example.com:0", "port number out of range 0"),
            ("example.com:70000", "port number out of range 70000"),
        ] {
            let (mut client, mut service) = tokio::io::duplex(64);
            let peer = tokio::spawn(async move {
                let mut byte = [0u8; 1];
                service.read(&mut byte).await.unwrap_or(0)
            });
            let error = match negotiate(&mut client, address, None).await {
                Ok(()) => panic!("{address}: the negotiation should have failed"),
                Err(error) => error,
            };
            assert_eq!(error.to_string(), wanted, "{address}");
            drop(client);
            assert_eq!(peer.await.unwrap_or(0), 0, "{address}: nothing may be sent");
        }
    }

    #[test]
    fn a_status_code_with_no_name_says_so() {
        assert_eq!(status_text(0x00), "succeeded");
        assert_eq!(status_text(0x06), "TTL expired");
        assert_eq!(status_text(0x42), "unknown code: 66");
    }
}
