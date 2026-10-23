//! The sandbox capability.
//!
//! One contract ([`contract::Runner`]) with a local process provider, an
//! endpoint that serves it over gRPC, and a remote client presenting the same
//! contract.
//!
//! # Honest capabilities
//!
//! The provider declares what it can actually enforce. The local runner has no
//! filesystem or network boundary, so a spec that asks for one is rejected with
//! `NotAvailable` — never quietly downgraded. A caller that wants confinement
//! points the deployment at a backend that has it.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod codes;
pub mod confinement;
pub mod contract;
pub mod endpoint;
pub mod local;
pub mod remote;
pub mod scope;

pub use codes::Code;
pub use confinement::{BwrapFactory, ConfinedRunner, Sandbox, SeatbeltFactory};
pub use contract::{
    CONTRACT, Capabilities, Chunk, Enforcement, ExecOptions, ExitStatus, Features, Handle, KIND,
    ReadResult, Runner, Session, SessionFeatures, SessionInfo, SessionSpec,
};
pub use local::LocalRunner;
pub use remote::RemoteRunner;
