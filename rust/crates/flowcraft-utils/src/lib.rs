//! The port's utility layer, mirroring `core/utils`.
//!
//! It starts with the decode path, because that is the surface every
//! configuration document crosses before anything else reads it -- and because
//! it is the part the conformance gate can already check end to end.

pub mod bm25;
pub mod decode;
pub mod filetype;
pub mod fsatomic;
pub mod lock;
pub mod pathsafe;
