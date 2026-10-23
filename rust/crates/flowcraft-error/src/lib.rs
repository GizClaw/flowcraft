//! The error taxonomy every FlowCraft crate shares.
//!
//! Fourteen categories. Thirteen mirror the Go side's `errdefs`; `Closed` is the
//! one addition (a closed handle, which the Go tree spells as a bare
//! `FAILED_PRECONDITION`). The category is part of the cross-language contract
//! because every transport maps it to a status code, while the message is
//! free-form. [`Error`] renders as its message alone so a diff against the Go
//! side compares like with like.
//!
//! This crate is the bottom of the dependency graph: it depends on nothing but
//! `thiserror`, and every other crate depends on it.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod error;

pub use error::{Error, ErrorKind, Result};
