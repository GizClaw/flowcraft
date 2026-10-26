//! `flowcraft-platformd` internals, exposed so tests can drive the same
//! assembly the binary uses.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]
pub mod abi;
pub mod admin;
pub mod assembly;
pub mod cli;
pub mod commands;
pub mod craft;
pub mod craft_stdio;
pub mod hosting;
pub mod logging;
pub mod secrets;
pub mod stub;

/// The rustls provider is process-wide and ambiguous in this build; pin it once
/// before any TLS configuration is built (see `flowcraft::kernel::tls`).
fn kernel_tls_provider() {
    flowcraft::kernel::tls::install_provider();
}
pub mod supervisor;
pub mod watcher;
