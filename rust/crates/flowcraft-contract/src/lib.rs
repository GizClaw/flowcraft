//! Contracts and data: the shapes core and the port have to agree on.
//!
//! What a caller can tell the two implementations apart by lives here -- the
//! text a value is written as, and the rules that decide it -- and the crates
//! that do work depend on it rather than restating it.

pub mod json;
