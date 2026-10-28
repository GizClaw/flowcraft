//! The HTTP transport: one POST, over hyper, with the trust store the caller supplies.
//!
//! `hyper` rather than a client framework, for two reasons that are both in this
//! repository: `kernel` already speaks hyper (so the TLS provider story is shared), and
//! a framework would bring its own `rustls` features — the workspace has a scar from
//! unifying `ring` (tonic) with `aws_lc_rs` (rmcp → reqwest), recorded in `Cargo.toml`.
//!
//! No baked-in root store. A provider endpoint is a deployment decision like any other,
//! and `kernel::tls` already reads a CA bundle from the document; a library that trusts
//! its own compiled-in roots would be making that decision behind the deployment's back.
//! [`HttpTransport::with_tls`] takes the store; the plain-`http` constructor exists for
//! loopback tests and for a provider a deployment chose to reach in the clear.

use std::sync::Arc;

use async_trait::async_trait;
use http_body_util::{BodyExt, Full};
use hyper::Request;
use hyper::body::Bytes;
use serde_json::Value;
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;
use tokio_rustls::rustls::{ClientConfig, RootCertStore};

use crate::error::{Error, Result};
use crate::inference::transport::Transport;

/// Performs provider requests over HTTP/1.1.
#[derive(Clone, Default)]
pub struct HttpTransport {
    /// `None` for plain HTTP (loopback tests, an endpoint a deployment chose in the
    /// clear); `Some` for TLS against the roots the deployment supplied.
    tls: Option<TlsConnector>,
}

impl HttpTransport {
    /// Plain HTTP. The caller has decided the endpoint needs no transport security —
    /// which is a decision, not a default.
    #[must_use]
    pub fn plain() -> Self {
        Self { tls: None }
    }

    /// HTTPS against `roots`.
    #[must_use]
    pub fn with_tls(roots: RootCertStore) -> Self {
        let config = ClientConfig::builder()
            .with_root_certificates(Arc::new(roots))
            .with_no_client_auth();
        Self {
            tls: Some(TlsConnector::from(Arc::new(config))),
        }
    }
}

impl std::fmt::Debug for HttpTransport {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The trust store is not printable and a connector has nothing to say that a
        // line would not misrepresent.
        f.debug_struct("inference.HttpTransport")
            .field("tls", &self.tls.is_some())
            .finish()
    }
}

#[async_trait]
impl Transport for HttpTransport {
    async fn post_json(&self, url: &str, headers: &[(&str, &str)], body: &Value) -> Result<Value> {
        let Target {
            secure,
            host,
            port,
            path,
        } = Target::parse(url)?;
        if secure && self.tls.is_none() {
            return Err(Error::validation(format!(
                "inference: {url} needs TLS, and this transport was built without a trust store"
            )));
        }
        // Origin-form (`POST /v1/responses`) with an explicit `Host`: a direct client
        // sends its path, not an absolute URL. The absolute form is for a proxy, and a
        // `Host` that disagrees with the connection is how a request reaches the wrong
        // virtual host.
        let authority = if (secure && port == 443) || (!secure && port == 80) {
            host.clone()
        } else {
            format!("{host}:{port}")
        };
        let request = Request::builder()
            .method("POST")
            .uri(&path)
            .header("host", authority)
            .header("content-type", "application/json")
            .header("accept", "application/json");
        let mut request = request;
        for (name, value) in headers {
            request = request.header(*name, *value);
        }
        let body = serde_json::to_vec(body)
            .map_err(|error| Error::internal(format!("inference: encode the body: {error}")))?;
        let request = request
            .body(Full::new(Bytes::from(body)))
            .map_err(|error| Error::internal(format!("inference: build the request: {error}")))?;

        let stream = TcpStream::connect((host.as_str(), port))
            .await
            .map_err(|error| {
                Error::not_available(format!("inference: connect {host}:{port}: {error}"))
            })?;
        // One connection per call for now: a pool is a later optimization, and a pool
        // around a handshake that can fail is a place to hide a failure.
        let response = match &self.tls {
            Some(tls) => {
                let server = tls
                    .connect(
                        host.clone().try_into().map_err(|error| {
                            Error::validation(format!(
                                "inference: {host:?} is not a server name: {error}"
                            ))
                        })?,
                        stream,
                    )
                    .await
                    .map_err(|error| {
                        Error::not_available(format!("inference: tls to {host}: {error}"))
                    })?;
                send(server, request).await?
            }
            None => send(stream, request).await?,
        };
        let (parts, body) = response.into_parts();
        let bytes = body
            .collect()
            .await
            .map_err(|error| Error::not_available(format!("inference: read the body: {error}")))?
            .to_bytes();
        if !parts.status.is_success() {
            // The provider's error body is what explains a refusal; truncating it keeps
            // a stack trace out of the message while keeping the point.
            let text = String::from_utf8_lossy(&bytes);
            let excerpt: String = text.chars().take(400).collect();
            return Err(Error::not_available(format!(
                "inference: {url} answered {}: {excerpt}",
                parts.status
            )));
        }
        serde_json::from_slice(&bytes)
            .map_err(|error| Error::internal(format!("inference: the answer is not JSON: {error}")))
    }
}

async fn send<S>(
    stream: S,
    request: Request<Full<Bytes>>,
) -> Result<hyper::Response<hyper::body::Incoming>>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Send + Unpin + 'static,
{
    let io = hyper_util::rt::TokioIo::new(stream);
    let (mut sender, connection) = hyper::client::conn::http1::handshake(io)
        .await
        .map_err(|error| Error::not_available(format!("inference: http handshake: {error}")))?;
    tokio::spawn(async move {
        // The connection future ends with the exchange; its error is the response's
        // error, which the caller already sees.
        let _ = connection.await;
    });
    sender
        .send_request(request)
        .await
        .map_err(|error| Error::not_available(format!("inference: send the request: {error}")))
}

/// The parts of a URL this transport needs.
struct Target {
    secure: bool,
    host: String,
    port: u16,
    path: String,
}

impl Target {
    /// Parse `scheme://host[:port][/path]`.
    ///
    /// Hand-written rather than a URL crate: the transport needs four fields, and a full
    /// parser would be a dependency bought for percent-decoding nobody asked for. What
    /// it does not support it refuses — a `userinfo` section, for instance, because a
    /// credential in a URL is a credential in a log.
    fn parse(url: &str) -> Result<Self> {
        let (scheme, rest) = url
            .split_once("://")
            .ok_or_else(|| Error::validation(format!("inference: {url:?} has no scheme")))?;
        let secure = match scheme {
            "https" => true,
            "http" => false,
            other => {
                return Err(Error::validation(format!(
                    "inference: {other}:// is not a transport this client speaks"
                )));
            }
        };
        let (authority, path) = match rest.find('/') {
            Some(at) => (&rest[..at], &rest[at..]),
            None => (rest, "/"),
        };
        if authority.contains('@') {
            return Err(Error::validation(format!(
                "inference: {url:?} carries credentials; configure them as headers instead"
            )));
        }
        let (host, port) = match authority.rsplit_once(':') {
            Some((host, port)) => {
                let port = port.parse::<u16>().map_err(|_| {
                    Error::validation(format!("inference: {url:?} has an invalid port"))
                })?;
                (host.to_owned(), port)
            }
            None => (authority.to_owned(), if secure { 443 } else { 80 }),
        };
        if host.is_empty() {
            return Err(Error::validation(format!("inference: {url:?} has no host")));
        }
        Ok(Self {
            secure,
            host,
            port,
            path: path.to_owned(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    /// A one-request server: it answers with `reply` and records what it was sent.
    async fn serve(reply: &'static str) -> (u16, tokio::task::JoinHandle<String>) {
        let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("bind");
        let port = listener.local_addr().expect("addr").port();
        let handle = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            // Read the head, then the body the head promised: a single `read` returns
            // whatever arrived first, which is the classic way a test like this passes
            // on one machine and sees half a request on another.
            let mut buffer: Vec<u8> = Vec::new();
            let mut chunk = [0u8; 1024];
            loop {
                let read = stream.read(&mut chunk).await.expect("read");
                if read == 0 {
                    break;
                }
                buffer.extend_from_slice(&chunk[..read]);
                let Some(head_end) = find_head_end(&buffer) else {
                    continue;
                };
                let head = String::from_utf8_lossy(&buffer[..head_end]).to_lowercase();
                let promised: usize = head
                    .lines()
                    .find_map(|line| line.strip_prefix("content-length:"))
                    .and_then(|value| value.trim().parse().ok())
                    .unwrap_or(0);
                if buffer.len() >= head_end + 4 + promised {
                    break;
                }
            }
            stream.write_all(reply.as_bytes()).await.expect("write");
            String::from_utf8_lossy(&buffer).into_owned()
        });
        (port, handle)
    }

    fn find_head_end(buffer: &[u8]) -> Option<usize> {
        buffer.windows(4).position(|window| window == b"\r\n\r\n")
    }

    #[tokio::test]
    async fn a_post_arrives_with_its_headers_and_body() {
        let (port, server) = serve(
            "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 11\r\n\r\n{\"ok\":true}",
        )
        .await;
        let transport = HttpTransport::plain();
        let answer = transport
            .post_json(
                &format!("http://127.0.0.1:{port}/v1/responses"),
                &[("authorization", "Bearer secret")],
                &serde_json::json!({"model": "gpt-5"}),
            )
            .await
            .expect("post");
        assert_eq!(answer["ok"], true);
        let request = server.await.expect("server");
        assert!(
            request.starts_with("POST /v1/responses HTTP/1.1"),
            "{request}"
        );
        assert!(
            request
                .to_lowercase()
                .contains("authorization: bearer secret"),
            "{request}"
        );
        assert!(request.contains("\"model\":\"gpt-5\""), "{request}");
    }

    /// A provider refusal is an error with the provider's own words in it, not a silent
    /// `Ok` with an empty body.
    #[tokio::test]
    async fn a_non_success_status_is_an_error_with_the_body() {
        let body = "{\"error\":{\"message\":\"no such model\"}}";
        let reply: &'static str = Box::leak(
            format!(
                "HTTP/1.1 404 Not Found\r\ncontent-type: application/json\r\ncontent-length: {}\r\n\r\n{body}",
                body.len()
            )
            .into_boxed_str(),
        );
        let (port, _server) = serve(reply).await;
        let error = HttpTransport::plain()
            .post_json(
                &format!("http://127.0.0.1:{port}/v1/responses"),
                &[],
                &serde_json::json!({}),
            )
            .await
            .expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        assert!(error.to_string().contains("404"), "{error}");
        assert!(error.to_string().contains("no such model"), "{error}");
    }

    /// The URL parser refuses what it does not speak instead of guessing.
    #[test]
    fn a_url_is_parsed_or_refused() {
        let target = Target::parse("https://api.example.com/v1/x").expect("https");
        assert!(target.secure);
        assert_eq!(target.host, "api.example.com");
        assert_eq!(target.port, 443);
        assert_eq!(target.path, "/v1/x");

        let target = Target::parse("http://127.0.0.1:8080").expect("default path");
        assert_eq!(target.port, 8080);
        assert_eq!(target.path, "/");

        assert!(Target::parse("ftp://x/y").is_err());
        assert!(Target::parse("api.example.com").is_err());
        // A credential in a URL is a credential in a log.
        assert!(Target::parse("https://user:pass@api.example.com/x").is_err());
    }

    /// A secure endpoint with no trust store is refused before a socket is opened: the
    /// alternative is a plaintext request to a port expecting TLS.
    #[tokio::test]
    async fn https_without_a_trust_store_is_refused() {
        let error = HttpTransport::plain()
            .post_json("https://api.example.com/v1", &[], &serde_json::json!({}))
            .await
            .expect_err("refused");
        assert!(error.to_string().contains("needs TLS"), "{error}");
    }
}
