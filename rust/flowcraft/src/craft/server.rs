//! The gRPC adapter: one `craft_server::Craft` over any [`CraftHandler`].
//!
//! Everything the ABI says about *calls* rather than about the craft lives here:
//! the bootstrap token that every RPC after `Describe` must carry, the rule that
//! a closed session answers `NotFound`, and the mapping from the platform's error
//! taxonomy to a gRPC status. A transport that carries the adapter adds nothing
//! but bytes.

use std::collections::BTreeSet;
use std::pin::Pin;
use std::sync::{Arc, Mutex};

use futures_util::{Stream, StreamExt as _};
use tonic::{Code, Request, Response, Status};

use crate::craft::handler::{CraftHandler, CraftStream};
use crate::error::Error;
use crate::proto::craft as pb;
use crate::proto::craft::craft_server;

/// Serves one [`CraftHandler`] as the `Craft` service.
///
/// A host builds it, optionally installs the bootstrap token, and hands it to
/// `craft_server::CraftServer` over whichever listener it owns.
pub struct Adapter {
    handler: Arc<dyn CraftHandler>,
    /// `None` accepts every caller, which is what a host whose transport already
    /// establishes trust wants (an in-process craft, or a socket only the daemon
    /// can reach).
    token: Option<String>,
    /// Sessions that answered `Close`. A second call against one is `NotFound`
    /// rather than a handler-specific guess.
    closed: Mutex<BTreeSet<String>>,
}

impl Adapter {
    /// Wrap a handler. No token: see [`Adapter::with_token`].
    #[must_use]
    pub fn new(handler: Arc<dyn CraftHandler>) -> Self {
        Self {
            handler,
            token: None,
            closed: Mutex::new(BTreeSet::new()),
        }
    }

    /// Require `x-flowcraft-craft-token` on every call after `Describe`.
    ///
    /// This is the second gate, not the only one: a child craft is also reachable
    /// only through a `0600` socket, and a remote one must additionally be behind
    /// TLS. The token says *which* craft this is; the transport says who may
    /// connect.
    #[must_use]
    pub fn with_token(mut self, token: impl Into<String>) -> Self {
        self.token = Some(token.into());
        self
    }

    /// The bootstrap-token rule: `Describe` is the handshake, everything after it
    /// carries the token the platform minted for this craft.
    fn authorize<T>(&self, request: &Request<T>) -> std::result::Result<(), Status> {
        let Some(expected) = &self.token else {
            return Ok(());
        };
        let presented = request
            .metadata()
            .get("x-flowcraft-craft-token")
            .and_then(|value| value.to_str().ok());
        if presented == Some(expected.as_str()) {
            Ok(())
        } else {
            Err(Status::unauthenticated("craft: missing or wrong token"))
        }
    }

    fn ensure_open(&self, session: &str) -> std::result::Result<(), Status> {
        let closed = self
            .closed
            .lock()
            .map_err(|_| Status::internal("craft: session state poisoned"))?;
        if closed.contains(session) {
            return Err(Status::not_found(format!(
                "craft: session {session:?} is closed"
            )));
        }
        Ok(())
    }

    fn note_closed(&self, session: &str) {
        if let Ok(mut closed) = self.closed.lock() {
            closed.insert(session.to_owned());
        }
    }
}

/// The category as a coarse status code.
///
/// The structured `flowcraft.error.v1` payload that capability endpoints add is
/// not part of this ABI yet: a craft call answers with a code and a message, and
/// the platform's client reads the category back out of the code.
fn status(error: Error) -> Status {
    Status::new(Code::from_i32(error.kind().grpc_code()), error.to_string())
}

type RunStream =
    Pin<Box<dyn Stream<Item = std::result::Result<pb::RunEvent, Status>> + Send + 'static>>;

#[tonic::async_trait]
impl craft_server::Craft for Adapter {
    type RunStream = RunStream;

    async fn describe(
        &self,
        _request: Request<pb::DescribeRequest>,
    ) -> std::result::Result<Response<pb::DescribeResponse>, Status> {
        let info = self.handler.describe().await.map_err(status)?;
        Ok(Response::new(pb::DescribeResponse { info: Some(info) }))
    }

    async fn start(
        &self,
        request: Request<pb::StartRequest>,
    ) -> std::result::Result<Response<pb::StartResponse>, Status> {
        self.authorize(&request)?;
        let response = self
            .handler
            .start(request.into_inner())
            .await
            .map_err(status)?;
        Ok(Response::new(response))
    }

    async fn run(
        &self,
        request: Request<pb::RunRequest>,
    ) -> std::result::Result<Response<Self::RunStream>, Status> {
        self.authorize(&request)?;
        let request = request.into_inner();
        self.ensure_open(&request.session)?;
        let events: CraftStream = self.handler.run(request);
        Ok(Response::new(Box::pin(
            events.map(|item| item.map_err(status)),
        )))
    }

    async fn steer(
        &self,
        request: Request<pb::SteerRequest>,
    ) -> std::result::Result<Response<pb::SteerResponse>, Status> {
        self.authorize(&request)?;
        let request = request.into_inner();
        self.ensure_open(&request.session)?;
        let response = self.handler.steer(request).await.map_err(status)?;
        Ok(Response::new(response))
    }

    async fn renew(
        &self,
        request: Request<pb::RenewRequest>,
    ) -> std::result::Result<Response<pb::RenewResponse>, Status> {
        self.authorize(&request)?;
        let request = request.into_inner();
        self.ensure_open(&request.session)?;
        let response = self.handler.renew(request).await.map_err(status)?;
        Ok(Response::new(response))
    }

    async fn cancel(
        &self,
        request: Request<pb::CancelRequest>,
    ) -> std::result::Result<Response<pb::CancelResponse>, Status> {
        self.authorize(&request)?;
        let request = request.into_inner();
        self.ensure_open(&request.session)?;
        let response = self.handler.cancel(request).await.map_err(status)?;
        Ok(Response::new(response))
    }

    async fn close(
        &self,
        request: Request<pb::CloseRequest>,
    ) -> std::result::Result<Response<pb::CloseResponse>, Status> {
        self.authorize(&request)?;
        let request = request.into_inner();
        let response = self.handler.close(request.clone()).await.map_err(status)?;
        self.note_closed(&request.session);
        Ok(Response::new(response))
    }
}
