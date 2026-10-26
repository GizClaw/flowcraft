//! The craft side of the `flowcraft.craft/v1` ABI.
//!
//! The contract in `proto/flowcraft/craft/v1/craft.proto` has two halves. This
//! module is the craft's: what a craft implements
//! ([`CraftHandler`]) and the adapter that serves it over gRPC ([`Adapter`]).
//! The platform's half is [`crate::client::craft`].
//!
//! # Why a layer of its own
//!
//! A craft is not a capability. A capability is reached through a grant that the
//! platform mints and the catalog authorizes; a craft *receives* those grants and
//! runs the agent loop behind them. So this module depends on the contract
//! ([`crate::proto`]) and the taxonomy ([`crate::error`]) and on nothing else:
//! no kernel, no transport policy, no capability.
//!
//! # Three bearers, one implementation
//!
//! A craft built into the daemon, a child process on the same machine, and a
//! process somewhere on the network all present the same contract. The bearer
//! decides where authentication sits — an in-process craft is already inside the
//! trust boundary, a child is a file-permission plus a token, a remote craft
//! needs TLS and a pre-shared token — but the seven calls, the session, and the
//! credit accounting are one implementation, which is what this module makes
//! possible.

mod handler;
mod server;

pub use handler::{CraftHandler, CraftStream};
pub use server::Adapter;
