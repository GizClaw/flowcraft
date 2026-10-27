//! The dev/test double for the craft side of the ABI.
//!
//! `flowcraftd craft-stub` serves this behind the generic adapter. It exists so
//! the platform side — the client, the supervisors, the conformance suite — can
//! be exercised without an agent loop, and every field is a way to make a test
//! observe one behaviour rather than a product feature.

use std::path::PathBuf;

use flowcraft::craft::{CraftHandler, CraftStream};
use flowcraft::error::Result;
use flowcraft::proto::craft as pb;

/// The session every stub answers with. One instance serves one session, which
/// is enough for everything the platform does before an agent loop exists.
const SESSION: &str = "stub-session";

/// The handler behind `flowcraftd craft-stub`.
pub struct StubCraft {
    name: String,
    abi_version: String,
    provides: Vec<String>,
    /// Append one line per `Renew`, so a test can prove the platform renews
    /// before a lease expires instead of discovering it after it has.
    renew_log: Option<PathBuf>,
    /// Emit a delta with no body: the platform must reject it, and this is how
    /// that path is reached from a real gRPC call.
    bad_envelope: bool,
}

impl StubCraft {
    /// Build the double.
    #[must_use]
    pub fn new(
        name: impl Into<String>,
        abi_version: Option<&str>,
        provides: Option<&str>,
        renew_log: Option<&str>,
        bad_envelope: bool,
    ) -> Self {
        Self {
            name: name.into(),
            abi_version: abi_version.unwrap_or(crate::abi::ABI_VERSION).to_owned(),
            provides: provides.map(|cap| vec![cap.to_owned()]).unwrap_or_default(),
            renew_log: renew_log.map(PathBuf::from),
            bad_envelope,
        }
    }
}

#[tonic::async_trait]
impl CraftHandler for StubCraft {
    async fn describe(&self) -> Result<pb::CraftInfo> {
        Ok(pb::CraftInfo {
            name: self.name.clone(),
            version: env!("CARGO_PKG_VERSION").to_owned(),
            requires: Vec::new(),
            provides: self.provides.clone(),
            tenancy: "single".to_owned(),
            abi_version: self.abi_version.clone(),
            abi_versions: Vec::new(),
            input_types: vec!["stub.text".to_owned()],
            event_types: vec!["stub.text".to_owned()],
        })
    }

    async fn start(&self, request: pb::StartRequest) -> Result<pb::StartResponse> {
        Ok(pb::StartResponse {
            session: SESSION.to_owned(),
            grants: request.grants,
        })
    }

    fn run(&self, _request: pb::RunRequest) -> CraftStream {
        let payload = if self.bad_envelope {
            pb::Envelope { body: None }
        } else {
            crate::abi::delta_text("hello from the stub")
        };
        let events: Vec<Result<pb::RunEvent>> = vec![
            Ok(pb::RunEvent {
                seq: 1,
                r#type: "delta".to_owned(),
                payload: Some(payload),
                error: String::new(),
                credit: 0,
            }),
            Ok(pb::RunEvent {
                seq: 2,
                r#type: "done".to_owned(),
                payload: None,
                error: String::new(),
                credit: 0,
            }),
        ];
        Box::pin(tokio_stream::iter(events))
    }

    async fn steer(&self, _request: pb::SteerRequest) -> Result<pb::SteerResponse> {
        Ok(pb::SteerResponse {})
    }

    async fn renew(&self, request: pb::RenewRequest) -> Result<pb::RenewResponse> {
        if let Some(path) = &self.renew_log {
            use std::io::Write as _;
            if let Ok(mut file) = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(path)
            {
                let _ = writeln!(file, "renew {}", request.session);
            }
        }
        Ok(pb::RenewResponse {
            grants: request.grants,
        })
    }

    async fn cancel(&self, _request: pb::CancelRequest) -> Result<pb::CancelResponse> {
        Ok(pb::CancelResponse {})
    }

    async fn close(&self, _request: pb::CloseRequest) -> Result<pb::CloseResponse> {
        Ok(pb::CloseResponse {})
    }
}
