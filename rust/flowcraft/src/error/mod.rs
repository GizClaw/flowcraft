//! The error taxonomy every FlowCraft crate shares.
//!
//! Fourteen categories. Thirteen mirror the Go side's `errdefs`; `Closed` is the
//! one addition (a closed handle, which the Go tree spells as a bare
//! `FAILED_PRECONDITION`). The category is part of the cross-language contract,
//! and it travels on the wire itself (`flowcraft.error.v1`) because two pairs of
//! categories share a gRPC status code — a code alone cannot name them.
//! [`Error`] renders as its message alone so a diff against the Go side compares
//! like with like.
//!
//! This crate is the bottom of the dependency graph: it depends on nothing but
//! `thiserror`, and every other crate depends on it. It deliberately does not
//! know its own wire encoding — the conversion lives in `flowcraft-client`, so a
//! crate that only names an error never compiles protobuf.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod detail;
pub mod taxonomy;

pub use self::taxonomy::{Error, ErrorKind, Result};
pub use detail::{Detail, Retry};
