//! Text retrieval primitives.
//!
//! The crate is a library of scoring kernels, not a search service: it knows
//! how to tokenize text and how to rank documents against a query, and nothing
//! about tools, memory, skills, or how documents are stored. Those policies
//! live in the crates that consume it.
//!
//! * [`tokenize()`] is the shared splitter: lowercased word runs, plus CJK
//!   characters and adjacent bigrams.
//! * [`bm25`] is an immutable index over a fixed document set.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod bm25;
pub mod tokenize;

pub use bm25::{Hit, Index, TermDoc};
pub use tokenize::tokenize;
