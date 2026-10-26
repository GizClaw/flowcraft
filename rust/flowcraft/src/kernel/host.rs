//! Listener creation for capability endpoints.
//!
//! Transport, socket paths, permissions, and TLS material are host policy, not
//! document settings: a resource names an endpoint, and the host decides where
//! it actually binds and whether it is wrapped in TLS.

#[cfg(unix)]
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::Path;
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::logerr;
use tokio::net::TcpListener;
#[cfg(unix)]
use tokio::net::UnixListener;
use tokio_stream::wrappers::TcpListenerStream;
#[cfg(unix)]
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;
use tonic::transport::server::Router;

use crate::kernel::catalog::StaticCatalog;
use crate::kernel::config::{Target, parse_target};
use crate::kernel::secret_provider::{Chain, SecretProvider};
use crate::kernel::tls;

/// A bound endpoint listener.
pub enum Listener {
    /// A unix-domain socket.
    #[cfg(unix)]
    Unix(UnixListener),
    /// A TCP socket, served with TLS.
    Tcp(TcpListener),
}

impl std::fmt::Debug for Listener {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            #[cfg(unix)]
            Self::Unix(_) => f.write_str("Listener::Unix"),
            Self::Tcp(_) => f.write_str("Listener::Tcp"),
        }
    }
}

impl Listener {
    /// Serve the configured routes until `shutdown` resolves.
    ///
    /// Tonic stops accepting on the signal and waits for in-flight requests, so
    /// a reload does not cut a call in half.
    pub async fn serve<F>(self, router: BoundRouter, shutdown: F) -> Result<()>
    where
        F: std::future::Future<Output = ()> + Send + 'static,
    {
        let outcome = match self {
            #[cfg(unix)]
            Self::Unix(listener) => {
                router
                    .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown)
                    .await
            }
            Self::Tcp(listener) => {
                router
                    .serve_with_incoming_shutdown(TcpListenerStream::new(listener), shutdown)
                    .await
            }
        };
        outcome.map_err(|error| Error::internal(format!("endpoint server stopped: {error}")))
    }
}

/// One running endpoint: the accept loop plus its graceful shutdown handle.
pub struct Serving {
    shutdown: Option<tokio::sync::oneshot::Sender<()>>,
    handle: Option<tokio::task::JoinHandle<()>>,
}

impl std::fmt::Debug for Serving {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Serving")
    }
}

impl Serving {
    /// Spawn the accept loop for `router` on `listener`.
    #[must_use]
    pub fn spawn(router: BoundRouter, listener: Listener) -> Self {
        let (shutdown, signal) = tokio::sync::oneshot::channel();
        let handle = tokio::spawn(async move {
            if let Err(failure) = listener
                .serve(router, async move {
                    let _ = signal.await;
                })
                .await
            {
                logerr!(error, failure, "endpoint stopped");
            }
        });
        Self {
            shutdown: Some(shutdown),
            handle: Some(handle),
        }
    }

    /// Stop accepting, wait for in-flight requests, and join the loop.
    pub async fn shutdown(mut self) -> Result<()> {
        if let Some(shutdown) = self.shutdown.take() {
            let _ = shutdown.send(());
        }
        if let Some(handle) = self.handle.take() {
            match tokio::time::timeout(std::time::Duration::from_secs(10), handle).await {
                Ok(_) => {}
                Err(_) => {
                    return Err(Error::timeout(
                        "endpoint did not drain within 10s; still serving",
                    ));
                }
            }
        }
        Ok(())
    }
}

/// The server `bind` prepares: the transport plus the trace middleware.
///
/// Named so a capability crate can hold one without spelling out the layer
/// stack, and so adding a middleware later is not a breaking change.
pub type BoundServer = Server<tower::layer::util::Stack<TraceLayer, tower::layer::util::Identity>>;

/// The router a [`BoundServer`] produces: the same layer stack, applied.
pub type BoundRouter = Router<tower::layer::util::Stack<TraceLayer, tower::layer::util::Identity>>;

/// Server-side trace context: one span per RPC, parented by `traceparent`.
///
/// The span is created in `call` and the request future runs inside it, so every
/// record and every nested span of one request shares the caller's trace id. A
/// request without a `traceparent` starts a new trace instead of inventing a
/// parent — a fabricated link is worse than a broken one.
#[derive(Debug, Clone, Copy, Default)]
pub struct TraceLayer;

impl<S> tower::Layer<S> for TraceLayer {
    type Service = TraceService<S>;

    fn layer(&self, inner: S) -> Self::Service {
        TraceService { inner }
    }
}

/// The service [`TraceLayer`] builds.
#[derive(Debug, Clone)]
pub struct TraceService<S> {
    inner: S,
}

impl<S, B> tower::Service<http::Request<B>> for TraceService<S>
where
    S: tower::Service<http::Request<B>>,
{
    type Response = S::Response;
    type Error = S::Error;
    type Future = tracing::instrument::Instrumented<S::Future>;

    fn poll_ready(
        &mut self,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::result::Result<(), Self::Error>> {
        self.inner.poll_ready(cx)
    }

    fn call(&mut self, request: http::Request<B>) -> Self::Future {
        use tracing::Instrument as _;

        // The parent has to be attached before the span is first materialised;
        // from there the SDK owns it.
        let path = request.uri().path();
        let span = tracing::info_span!(
            "rpc",
            flowcraft.capability = %capability_of(path),
            rpc.method = %path,
            // Read by tracing-opentelemetry, not by us: these set the exported
            // span's name and kind.
            otel.name = %path,
            otel.kind = "server",
        );
        crate::telemetry::set_parent(&span, &trace_headers(request.headers()));
        self.inner.call(request).instrument(span)
    }
}

/// `workspace.Workspace/v1` out of `/workspace.Workspace/v1/Save`.
///
/// The capability is the first two path segments of a gRPC method, which is
/// exactly the contract identifier a record or a dashboard filters on.
fn capability_of(path: &str) -> &str {
    let trimmed = path.trim_start_matches('/');
    match trimmed.match_indices('/').nth(1) {
        Some((index, _)) => &trimmed[..index],
        None => trimmed,
    }
}

/// The `traceparent` headers of one request, in the shape the propagator wants.
fn trace_headers(headers: &http::HeaderMap) -> crate::telemetry::propagation::Headers {
    let mut map = crate::telemetry::propagation::Headers::new();
    for (name, value) in headers {
        // `traceparent` is the only header the W3C context travels in.
        if name == "traceparent"
            && let Ok(text) = value.to_str()
        {
            map.insert(name.as_str().to_owned(), text.to_owned());
        }
    }
    map
}

/// Owns listener creation for capability endpoints.
#[derive(Clone)]
pub struct EndpointHost {
    catalog: Arc<StaticCatalog>,
    secrets: Arc<dyn SecretProvider>,
}

impl std::fmt::Debug for EndpointHost {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("EndpointHost")
            .field("endpoints", &self.catalog.len())
            .finish_non_exhaustive()
    }
}

impl EndpointHost {
    /// Build a host over a catalog, with no bootstrap secrets.
    #[must_use]
    pub fn new(catalog: Arc<StaticCatalog>) -> Self {
        Self {
            catalog,
            secrets: Arc::new(Chain::default()),
        }
    }

    /// Install the bootstrap secret provider used for TLS material.
    #[must_use]
    pub fn with_secrets(mut self, secrets: Arc<dyn SecretProvider>) -> Self {
        self.secrets = secrets;
        self
    }

    /// The catalog this host resolves names against.
    #[must_use]
    pub fn catalog(&self) -> Arc<StaticCatalog> {
        self.catalog.clone()
    }

    /// Bind an endpoint and prepare its server.
    ///
    /// A unix endpoint is created owner-only; a stale socket left by a crashed
    /// process is removed first, and anything that is not a socket is reported
    /// instead of replaced. A `dns://` endpoint binds TCP and requires TLS, so
    /// the returned server already carries the acceptor.
    pub fn bind(&self, endpoint: &str) -> Result<(BoundServer, Listener)> {
        let descriptor = self.catalog.resolve(endpoint)?;
        let target = parse_target(&descriptor.target)?;
        match target {
            #[cfg(unix)]
            Target::Unix(path) => {
                if descriptor.tls.is_some() {
                    return Err(Error::validation(format!(
                        "endpoint {endpoint:?}: tls is only meaningful on a dns target"
                    )));
                }
                let listener = bind_unix(endpoint, &path)?;
                Ok((
                    Server::builder().layer(TraceLayer),
                    Listener::Unix(listener),
                ))
            }
            // A unix socket is a unix mechanism. A host elsewhere binds a `dns`
            // target, which the arm below already serves over TCP + TLS.
            #[cfg(not(unix))]
            Target::Unix(path) => Err(Error::not_available(format!(
                "endpoint {endpoint:?}: a unix socket target ({}) requires unix; use a dns target",
                path.display()
            ))),
            Target::Dns(address) => {
                let Some(config) = descriptor.tls.as_ref() else {
                    return Err(Error::validation(format!(
                        "endpoint {endpoint:?}: a dns target requires tls"
                    )));
                };
                let Some(server_tls) = tls::server(config, &*self.secrets)? else {
                    return Err(Error::validation(format!(
                        "endpoint {endpoint:?}: tls block is empty"
                    )));
                };
                let listener = bind_tcp(endpoint, &address)?;
                let server = Server::builder()
                    .layer(TraceLayer)
                    .tls_config(server_tls)
                    .map_err(|error| {
                        Error::validation(format!("endpoint {endpoint:?}: {error}"))
                    })?;
                Ok((server, Listener::Tcp(listener)))
            }
        }
    }
}

#[cfg(unix)]
fn bind_unix(endpoint: &str, path: &Path) -> Result<UnixListener> {
    prepare_dir(path)?;
    if path.exists() {
        let metadata = std::fs::metadata(path)?;
        if !metadata.file_type().is_socket() {
            return Err(Error::conflict(format!(
                "endpoint {endpoint:?}: {:?} exists and is not a socket",
                path
            )));
        }
        std::fs::remove_file(path)?;
    }
    let listener = std::os::unix::net::UnixListener::bind(path)?;
    listener.set_nonblocking(true)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    UnixListener::from_std(listener)
        .map_err(|error| Error::internal(format!("endpoint {endpoint:?}: {error}")))
}

fn bind_tcp(endpoint: &str, address: &str) -> Result<TcpListener> {
    let listener = std::net::TcpListener::bind(address)
        .map_err(|error| Error::not_available(format!("endpoint {endpoint:?}: {error}")))?;
    listener.set_nonblocking(true)?;
    TcpListener::from_std(listener)
        .map_err(|error| Error::internal(format!("endpoint {endpoint:?}: {error}")))
}

#[cfg(unix)]
fn prepare_dir(path: &Path) -> Result<()> {
    let Some(parent) = path.parent() else {
        return Err(Error::validation(format!(
            "socket path {path:?} has no parent directory"
        )));
    };
    if parent.as_os_str().is_empty() {
        return Ok(());
    }
    if !parent.exists() {
        std::fs::create_dir_all(parent)?;
        std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use crate::kernel::config::{Config, EndpointConfig, Mode, RemoteConfig};
    use std::collections::BTreeMap;

    fn host(dir: &std::path::Path) -> EndpointHost {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: format!("unix://{}", dir.join("ws.sock").display()),
                        tenant: "a".to_owned(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            ..Config::default()
        };
        config.validate().unwrap();
        EndpointHost::new(Arc::new(StaticCatalog::from_config(&config)))
    }

    #[tokio::test]
    async fn binds_the_configured_socket_with_owner_only_permissions() {
        let dir = tempfile::tempdir().unwrap();
        let endpoint_host = host(dir.path());
        let (_, listener) = endpoint_host.bind("ws").unwrap();
        let path = dir.path().join("ws.sock");
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        drop(listener);
    }

    #[tokio::test]
    async fn replaces_a_stale_socket() {
        let dir = tempfile::tempdir().unwrap();
        let endpoint_host = host(dir.path());
        drop(endpoint_host.bind("ws").unwrap());
        assert!(dir.path().join("ws.sock").exists());
        drop(endpoint_host.bind("ws").unwrap());
    }

    #[tokio::test]
    async fn refuses_to_replace_a_regular_file() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("ws.sock"), b"not a socket").unwrap();
        let endpoint_host = host(dir.path());
        let error = endpoint_host.bind("ws").unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn unknown_endpoint_is_not_found() {
        let dir = tempfile::tempdir().unwrap();
        let endpoint_host = host(dir.path());
        assert_eq!(
            endpoint_host.bind("nope").err().unwrap().kind(),
            crate::error::ErrorKind::NotFound
        );
    }

    #[tokio::test]
    async fn a_dns_target_without_tls_is_refused_at_bind_time() {
        // Config validation already rejects it; `bind` refuses too, so a host
        // that built a catalog by hand cannot serve plaintext by accident.
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "farm".to_owned(),
                    EndpointConfig {
                        target: "dns:///127.0.0.1:1".to_owned(),
                        tenant: String::new(),
                        capabilities: Vec::new(),
                        tls: None,
                    },
                )]),
            },
            ..Config::default()
        };
        let endpoint_host = EndpointHost::new(Arc::new(StaticCatalog::from_config(&config)));
        let error = endpoint_host.bind("farm").err().unwrap();
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
    }
}

/// A host off unix: the catalog still parses every target, and `bind` refuses
/// the ones this platform cannot serve instead of quietly serving elsewhere.
#[cfg(all(test, not(unix)))]
mod non_unix {
    use super::*;
    use crate::kernel::catalog::StaticCatalog;
    use crate::kernel::config::{Config, EndpointConfig, Mode, RemoteConfig};
    use std::collections::BTreeMap;

    fn host(dir: &std::path::Path) -> EndpointHost {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: format!("unix://{}", dir.join("ws.sock").display()),
                        tenant: "a".to_owned(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            ..Config::default()
        };
        config.validate().unwrap();
        EndpointHost::new(Arc::new(StaticCatalog::from_config(&config)))
    }

    #[tokio::test]
    async fn a_unix_target_is_refused_with_not_available() {
        let dir = tempfile::tempdir().unwrap();
        let error = host(dir.path()).bind("ws").unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        assert!(
            error.to_string().contains("requires unix"),
            "the refusal has to say why: {error}"
        );
    }
}
