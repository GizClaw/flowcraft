//! The checkpoint capability.
//!
//! A store keyed by execution id, served over the existing
//! `flowcraft.checkpoint.v1` wire. The wire's `Save` carries the checkpoint
//! payload only — the id lives *inside* the payload — so this crate reads one
//! field out of it and stores the bytes untouched. That keeps the Go client
//! interoperable without importing any Go-side type.
//!
//! The payload contract is therefore: JSON, with a non-empty `exec_id` string.
//! Everything else is opaque.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod contract;
pub mod endpoint;
pub mod local;
pub mod remote;
pub mod scope;

pub use contract::{CONTRACT, Handle, KIND, Store, extract_exec_id};
pub use local::LocalStore;
pub use remote::RemoteStore;
