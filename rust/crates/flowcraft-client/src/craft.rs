//! The platform-to-craft client.
//!
//! The craft runs the agent loop; the platform owns its session and hands it
//! grants. This module speaks only the ABI: it never interprets a craft's
//! internal graph, and every craft-specific payload stays opaque bytes.

use flowcraft_error::{Error, Result};
use flowcraft_proto::craft as pb;
use flowcraft_proto::craft::craft_client::CraftClient as Generated;
use tonic::transport::{Channel, ClientTlsConfig};
use tower::Layer as _;

use flowcraft_kernel::config::Target;
use flowcraft_kernel::transport;

/// A typed client for one craft instance.
#[derive(Clone)]
pub struct CraftClient {
    // The channel carries a client span, so a craft call is part of the trace
    // that issued it rather than a detached branch.
    inner: Generated<crate::trace::TraceService<Channel>>,
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
            inner: Generated::new(crate::trace::TraceLayer.layer(channel)),
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
                crate::trace::TraceLayer.layer(transport::connect(target, tls).await?),
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
            .map_err(crate::grpc::from_status)?;
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
            .map_err(crate::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Run one turn. The returned stream is the craft's event stream.
    pub async fn run(&self, request: pb::RunRequest) -> Result<tonic::Streaming<pb::RunEvent>> {
        let mut client = self.inner.clone();
        let response = client
            .run(self.request(request)?)
            .await
            .map_err(crate::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Feed the running turn.
    pub async fn steer(&self, request: pb::SteerRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .steer(self.request(request)?)
            .await
            .map_err(crate::grpc::from_status)?;
        Ok(())
    }

    /// Replace the session's grants before they expire.
    pub async fn renew(&self, request: pb::RenewRequest) -> Result<pb::RenewResponse> {
        let mut client = self.inner.clone();
        let response = client
            .renew(self.request(request)?)
            .await
            .map_err(crate::grpc::from_status)?;
        Ok(response.into_inner())
    }

    /// Stop the turn, keep the session.
    pub async fn cancel(&self, request: pb::CancelRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .cancel(self.request(request)?)
            .await
            .map_err(crate::grpc::from_status)?;
        Ok(())
    }

    /// Close the session.
    pub async fn close(&self, request: pb::CloseRequest) -> Result<()> {
        let mut client = self.inner.clone();
        client
            .close(self.request(request)?)
            .await
            .map_err(crate::grpc::from_status)?;
        Ok(())
    }
}
