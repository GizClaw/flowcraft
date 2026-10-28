//! What a craft implements.

use std::pin::Pin;

use async_trait::async_trait;
use futures_util::Stream;

use crate::error::Result;
use crate::proto::craft as pb;

/// One turn's events, as the craft produces them.
///
/// The taxonomy, not a gRPC status: a craft says *what* went wrong and the
/// adapter decides how that looks on the wire. It also means an in-process craft
/// never touches a transport type.
pub type CraftStream = Pin<Box<dyn Stream<Item = Result<pb::RunEvent>> + Send + 'static>>;

/// The craft side of the ABI.
///
/// Every method corresponds to one RPC. Authentication, the session-closed rule,
/// and the mapping to gRPC statuses are the adapter's job ([`crate::craft::Adapter`]),
/// so an implementation only answers the question it is asked — and an
/// in-process craft can be called directly with no wire at all.
#[async_trait]
pub trait CraftHandler: Send + Sync {
    /// Identify the craft: name, version, capabilities, ABI version, and the
    /// private envelope types it accepts and emits.
    async fn describe(&self) -> Result<pb::CraftInfo>;

    /// Open one session and take the grants the platform offers.
    ///
    /// The craft may narrow them; what it returns is what it holds.
    async fn start(&self, request: pb::StartRequest) -> Result<pb::StartResponse>;

    /// Execute one turn, emitting sequenced events until `done`.
    ///
    /// Returns a stream rather than a future because the events are produced as
    /// the turn runs: the caller's credit is a budget on how far ahead the craft
    /// may go, not a batch size.
    fn run(&self, request: pb::RunRequest) -> CraftStream;

    /// Feed the running turn: more input, or a correction.
    async fn steer(&self, request: pb::SteerRequest) -> Result<pb::SteerResponse>;

    /// Replace the session's grants with a fresh set before they expire.
    async fn renew(&self, request: pb::RenewRequest) -> Result<pb::RenewResponse>;

    /// Stop the running turn, keep the session.
    async fn cancel(&self, request: pb::CancelRequest) -> Result<pb::CancelResponse>;

    /// Close the session and release everything it owns.
    async fn close(&self, request: pb::CloseRequest) -> Result<pb::CloseResponse>;
}
