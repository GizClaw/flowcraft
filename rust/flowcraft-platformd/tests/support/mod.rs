//! Shared harness for the daemon's integration tests.
//!
//! Every spawned daemon gets its own identity file. Two reasons: a test must
//! never write to the identity of the machine it runs on, and two tests running
//! in parallel must not share one — the identity file is created with `O_EXCL`,
//! and a shared path would make that race part of the test.

#![allow(dead_code)]

use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};

/// The daemon under test.
pub const BIN: &str = env!("CARGO_BIN_EXE_flowcraftd");

/// The variable [`flowcraft::kernel::identity`] honours.
pub const IDENTITY_ENV: &str = "FLOWCRAFT_IDENTITY_PATH";

static COUNTER: AtomicU64 = AtomicU64::new(0);

/// A unique, not-yet-created identity path for one spawned process.
pub fn identity_path() -> PathBuf {
    std::env::temp_dir()
        .join(format!(
            "flowcraftd-test-{}-{}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ))
        .join("identity.json")
}

/// The environment pair a spawned daemon needs, as `CraftConfig::env` wants it.
pub fn identity_env() -> (String, String) {
    (
        IDENTITY_ENV.to_owned(),
        identity_path().display().to_string(),
    )
}

/// Point a synchronous command at an isolated identity.
pub fn isolated(mut command: std::process::Command) -> std::process::Command {
    command.env(IDENTITY_ENV, identity_path());
    command
}

/// Point an async command at an isolated identity.
pub fn isolated_async(mut command: tokio::process::Command) -> tokio::process::Command {
    command.env(IDENTITY_ENV, identity_path());
    command
}
