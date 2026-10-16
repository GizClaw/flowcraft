//! Contracts and data: the shapes core and the port have to agree on.
//!
//! What a caller can tell the two implementations apart by lives here -- the
//! text a value is written as, and the rules that decide it -- and the crates
//! that do work depend on it rather than restating it.

// The workspace sets missing_docs to warn; this crate tightens it to deny,
// because everything public here is something the other side of the port
// reads, and an undocumented item is a hole in the contract rather than a
// style nit. An attribute rather than a `[lints]` table because Cargo refuses
// to let a member combine `workspace = true` with overrides.
#![deny(missing_docs)]

pub mod error;
pub mod json;
pub mod provider;
