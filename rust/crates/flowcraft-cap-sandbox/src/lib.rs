//! The sandbox capability.
//!
//! One contract ([`contract::Runner`] / [`contract::Session`]) with a local
//! process provider, an endpoint that serves it over gRPC, and a remote client
//! presenting the same contract.
//!
//! # Honest capabilities
//!
//! A provider declares what it can actually enforce, and the declaration is
//! part of the wire contract. The local runner has no filesystem or network
//! boundary, so a spec that asks for one is rejected with `NotAvailable` —
//! never quietly downgraded. A caller that wants confinement points the
//! deployment at a backend that has it.
//!
//! # Output is pulled, not pushed
//!
//! [`contract::Session::read`] takes a **byte** cursor and blocks until there
//! is something to return, so a slow consumer cannot stall the child and a
//! reconnecting client resumes at the exact offset it consumed. The bounded
//! buffer drops the oldest chunks and reports a cursor below the retained range
//! as a gap; [`contract::Session::watch`] is the optional push view of the same
//! stream.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod confinement;
pub mod contract;
pub mod endpoint;
pub mod exec;
pub mod journal;
mod limits;
pub mod local;
mod net;
pub mod policy;
pub mod remote;
pub mod scope;
mod session;
mod wire;

pub use confinement::{BwrapFactory, ConfinedRunner, Sandbox, SandboxConfig, SeatbeltFactory};
pub use contract::{
    CONTRACT, Capabilities, Chunk, Enforcement, EnvPolicy, Event, EventStream, ExecOptions,
    ExitReason, ExitStatus, Features, Handle, JournalCapabilities, KIND, NetAction, NetMode,
    NetModes, NetPolicy, NetRule, ReadResult, ResourceLimits, Runner, Session, SessionFeatures,
    SessionInfo, SessionSpec, Signal, Stream, WriteMode, WriteModes,
};
pub use exec::{ExecResult, exec};
pub use local::LocalRunner;
pub use policy::{
    Allowlist, ApprovalRequest, Approver, ApproverFn, Chain, Decision, Escalation,
    EscalationDecision, EscalationGate, EscalationRules, Escalator, ExecRequest, Layer,
    LocalPolicy, Outcome, Predicate, RunnerRef, allow_commands, approval, classify_read_only,
    command_patterns, compose_local, defaults, denial, interactive, net_non_default, normalise,
    rule_for, workdir_outside_root,
};
pub use remote::RemoteRunner;
