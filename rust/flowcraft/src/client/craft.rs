//! The platform-to-craft client.
//!
//! The craft runs the agent loop; the platform owns its session and hands it
//! grants. This module speaks only the ABI: it never interprets a craft's
//! internal graph, and every craft-specific payload stays opaque bytes.

use crate::error::{Error, Result};
use crate::proto::craft as pb;
use crate::proto::craft::craft_client::CraftClient as Generated;
use tonic::transport::{Channel, ClientTlsConfig};
use tower::Layer as _;

use crate::kernel::config::Target;
use crate::kernel::transport;

/// A typed client for one craft instance.
#[derive(Clone)]
pub struct CraftClient {
    // The channel carries a client span, so a craft call is part of the trace
    // that issued it rather than a detached branch.
    inner: Generated<crate::client::trace::TraceService<Channel>>,
    token: Option<String>,
}

impl std::fmt::Debug for CraftClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("CraftClient")
    }
}

impl CraftClient {
    /// Wrap a channel.
    #[must_use]
    pub fn new(channel: Channel) -> Self {
        Self {
            inner: Generated::new(crate::client::trace::TraceLayer.layer(channel)),
            token: None,
        }
    }

    /// Connect to a craft over a unix socket (or TCP with TLS).
    pub async fn connect(target: &Target, tls: Option<ClientTlsConfig>) -> Result<Self> {
        Self::connect_with_token(target, tls, None).await
    }

    /// Connect and attach the platform-issued bootstrap token every call must
    /// carry.
    pub async fn connect_with_token(
        target: &Target,
        tls: Option<ClientTlsConfig>,
        token: Option<&str>,
    ) -> Result<Self> {
        Ok(Self {
            inner: Generated::new(
                crate::client::trace::TraceLayer.layer(transport::connect(target, tls).await?),
            ),
            token: token.map(str::to_owned),
        })
    }

    fn request<T>(&self, payload: T) -> Result<tonic::Request<T>> {
        let mut request = tonic::Request::new(payload);
        if let Some(token) = &self.token {
            let value =
                tonic::metadata::MetadataValue::try_from(token.as_str()).map_err(|error| {
                    Error::internal(format!("craft token is not valid metadata: {error}"))
                })?;
            request
                .metadata_mut()
                .insert("x-flowcraft-craft-token", value);
        }
        Ok(request)
    }

    /// Identify the craft.
    pub async fn describe(&self) -> Result<pb::CraftInfo> {
        let mut client = self.inner.clone();
        let response = client
            .describe(self.request(pb::DescribeRequest {})?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        response
            .into_inner()
            .info
            .ok_or_else(|| Error::internal("craft: Describe returned no info"))
    }

    /// Open a session with the grants the craft may use.
    pub async fn start(&self, request: pb::StartRequest) -> Result<pb::StartResponse> {
        let mut client = self.inner.clone();
        let response = client
            .start(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Run one turn. The returned stream is the craft's event stream.
    pub async fn run(&self, request: pb::RunRequest) -> Result<tonic::Streaming<pb::RunEvent>> {
        let mut client = self.inner.clone();
        let response = client
            .run(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Feed the running turn.
    pub async fn steer(&self, request: pb::SteerRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .steer(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(())
    }

    /// Replace the session's grants before they expire.
    pub async fn renew(&self, request: pb::RenewRequest) -> Result<pb::RenewResponse> {
        let mut client = self.inner.clone();
        let response = client
            .renew(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Stop the turn, keep the session.
    pub async fn cancel(&self, request: pb::CancelRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .cancel(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(())
    }

    /// Close the session.
    pub async fn close(&self, request: pb::CloseRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .close(self.request(request)?)
            .await
            .map_err(crate::client::grpc::from_status)?;
        Ok(())
    }
}

/// The platform's view of one craft, whichever way it is carried.
///
/// The ABI is the same in all three cases — the seven calls, one session, the
/// same envelope rules — and so is this type. Only the bearer differs:
///
/// - [`CraftSession::InProcess`] is a craft built into this daemon: no wire, no
///   token, no serialization, and a panic in it is this process's problem. The
///   ABI's closed-session rule lives in the wire adapter (`crate::craft::Adapter`),
///   so an in-process craft answers whatever its handler answers — the platform
///   owns the bookkeeping either way;
/// - [`CraftSession::Child`] is a process the daemon started (or a unit it
///   owns): a unix socket whose permissions admit only this user, plus the
///   bootstrap token handed to the child through its environment;
/// - [`CraftSession::Remote`] is a craft that owns its lifecycle: a `dns`
///   target, TLS, and a token the platform did not mint.
///
/// Every method answers with the platform's own types, so the caller never
/// learns which bearer it holds.
pub enum CraftSession {
    /// Built into this process.
    InProcess(std::sync::Arc<dyn crate::craft::CraftHandler>),
    /// A child process (or a unit) of this daemon.
    Child(CraftClient),
    /// A craft reached over the network.
    Remote(CraftClient),
}

impl CraftSession {
    /// The client behind a wire session, if there is one.
    #[must_use]
    pub fn client(&self) -> Option<&CraftClient> {
        match self {
            Self::InProcess(_) => None,
            Self::Child(client) | Self::Remote(client) => Some(client),
        }
    }

    /// Identify the craft.
    pub async fn describe(&self) -> Result<pb::CraftInfo> {
        match self {
            Self::InProcess(handler) => handler.describe().await,
            Self::Child(client) | Self::Remote(client) => client.describe().await,
        }
    }

    /// Open one session.
    pub async fn start(&self, request: pb::StartRequest) -> Result<pb::StartResponse> {
        match self {
            Self::InProcess(handler) => handler.start(request).await,
            Self::Child(client) | Self::Remote(client) => client.start(request).await,
        }
    }

    /// Execute one turn.
    ///
    /// The wire returns a gRPC streaming body and the in-process craft returns a
    /// stream directly; both become one [`crate::craft::CraftStream`] so a caller can be
    /// written against either.
    pub async fn run(&self, request: pb::RunRequest) -> Result<crate::craft::CraftStream> {
        use futures_util::StreamExt as _;
        match self {
            Self::InProcess(handler) => Ok(handler.run(request)),
            Self::Child(client) | Self::Remote(client) => {
                let stream = client.run(request).await?;
                Ok(Box::pin(stream.map(|event| {
                    event.map_err(crate::client::grpc::from_status)
                })))
            }
        }
    }

    /// Feed the running turn.
    pub async fn steer(&self, request: pb::SteerRequest) -> Result<()> {
        match self {
            Self::InProcess(handler) => handler.steer(request).await.map(|_| ()),
            Self::Child(client) | Self::Remote(client) => client.steer(request).await,
        }
    }

    /// Replace the session's grants.
    pub async fn renew(&self, request: pb::RenewRequest) -> Result<pb::RenewResponse> {
        match self {
            Self::InProcess(handler) => handler.renew(request).await,
            Self::Child(client) | Self::Remote(client) => client.renew(request).await,
        }
    }

    /// Stop the running turn.
    pub async fn cancel(&self, request: pb::CancelRequest) -> Result<()> {
        match self {
            Self::InProcess(handler) => handler.cancel(request).await.map(|_| ()),
            Self::Child(client) | Self::Remote(client) => client.cancel(request).await,
        }
    }

    /// Close the session.
    pub async fn close(&self, request: pb::CloseRequest) -> Result<()> {
        match self {
            Self::InProcess(handler) => handler.close(request).await.map(|_| ()),
            Self::Child(client) | Self::Remote(client) => client.close(request).await,
        }
    }
}
