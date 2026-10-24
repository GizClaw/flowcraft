//! The bridge entry point used by `cap-sandbox` on its own (tests, and any host
//! that does not ship the daemon's binary).
//!
//! A deployment normally points `net.bridge` at `flowcraftd`, whose `main` calls
//! [`flowcraft_cap_sandbox::net::bridge::run_if_invoked`] before anything else.
//! This binary exists so the capability is usable without the daemon.

fn main() {
    // Reached only when invoked with the marker; otherwise there is nothing to
    // do and the exit status says so.
    if !flowcraft_cap_sandbox::net::bridge::run_if_invoked() {
        eprintln!(
            "flowcraft-sandbox-bridge: run through bwrap as\n  \
             <this> {} --sock <proxy.sock> -- <cmd> [args…]",
            flowcraft_cap_sandbox::net::bridge::MARKER
        );
        std::process::exit(2);
    }
}
