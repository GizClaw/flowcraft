//! Wire types generated from `proto/`.
//!
//! The `.proto` files at the repository root are the source of truth, shared
//! with the Go SDK. The Rust bindings are committed under `generated/`, so a
//! build — a consumer's, docs.rs', or `cargo install` — needs neither `protoc`
//! nor a build script. `make rust-proto-gen` regenerates them from `proto/`,
//! and `make rust-proto-check` fails on drift, the way `make proto-check`
//! guards the Go stubs. The generator is `tools/proto-gen`, a `publish = false`
//! repository tool: it reads `proto/` from outside this package.
//!
//! One module per proto package: `flowcraft.<area>.v1` becomes `proto::<area>`;
//! a non-`v1` package appends its version (`flowcraft.foo.v2` → `foo_v2`) so
//! two versions can be served side by side during a migration. Adding a
//! contract under `proto/flowcraft/<area>/v1/` needs no change here — see
//! `tools/proto-gen`.

// A capability's wire types are its contract: the doc comments live in the
// `.proto` files, and generated bodies have no `unwrap` to fix, so the
// crate-wide lints below are switched off for this tree only.
#[allow(
    missing_docs,
    // The prose lives in the `.proto` files: a `<vendor>`-style placeholder in a
    // comment is not ours to fix here.
    rustdoc::all,
    clippy::all,
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::future_not_send
)]
mod generated {
    include!("generated/mod.rs");
}

pub use generated::*;
