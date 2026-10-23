//! Generated bindings for the wire contracts under `proto/`.
//!
//! The `.proto` files are the source of truth and live outside every language
//! tree, shared with the Go SDK. This crate compiles all of them at build time
//! (`build.rs` walks `proto/flowcraft/…`), so adding a contract needs no change
//! here.
//!
//! Module naming: `flowcraft.<area>.v1` becomes `flowcraft_proto::<area>`; a
//! non-`v1` package appends its version (`flowcraft.foo.v2` → `foo_v2`) so
//! both can be served during a migration.

include!(concat!(env!("OUT_DIR"), "/generated.rs"));
