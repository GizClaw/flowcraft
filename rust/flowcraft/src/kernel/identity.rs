//! Machine identity.
//!
//! One install identifier per machine, shared by every application that embeds
//! the platform. It answers "which machine produced this record?" without
//! being a device fingerprint: the value is random, it is not derived from a MAC
//! address, a disk serial, `/etc/machine-id` or a hostname, and deleting the
//! file mints a new one.
//!
//! # Why this lives in the kernel and not in telemetry
//!
//! Telemetry only *renders* the identity. Provisioning, permission and reset are
//! policy, so they belong with grants, TLS material and the rest of the control
//! plane — and other consumers (craft launch, the support bundle, a future
//! licence check) need the same value without pulling in a logging pipeline.
//!
//! # Scope
//!
//! A machine identity only ever has to be **readable**, never writable, by the
//! processes that use it. That is what makes "one per machine" work without
//! privilege: the installer (or root) writes it once, and every user-level
//! daemon on the machine reads the same file.
//!
//! | | path |
//! |---|---|
//! | Linux | `/var/lib/flowcraft/identity.json` |
//! | macOS | `/Library/Application Support/FlowCraft/identity.json` |
//! | Windows | `%ProgramData%\\FlowCraft\\identity.json` |
//!
//! When that file is absent — an unpackaged checkout, a sandboxed app, a
//! developer running `cargo run` — the identity is created in the per-user state
//! directory and labelled [`Scope::User`], never silently passed off as a
//! machine one. If even that fails, an in-memory identity is generated so the
//! process still starts.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::error::{Error, ErrorKind, Result};
use serde::{Deserialize, Serialize};

/// The environment variable that overrides the location (tests, packaged
/// deployments with an unusual layout).
pub const PATH_OVERRIDE: &str = "FLOWCRAFT_IDENTITY_PATH";

/// The document's on-disk schema version.
const SCHEMA: u32 = 1;

/// The file name inside whichever directory is chosen.
const FILE_NAME: &str = "identity.json";

/// Where an identity came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Scope {
    /// A machine-wide file written by the installer or root.
    Machine,
    /// A per-user file, used when no machine-wide one could be read.
    User,
    /// Never persisted: the process could not read or write any location.
    Ephemeral,
}

impl Scope {
    /// The wire spelling, used as the `flowcraft.install.scope` field.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Machine => "machine",
            Self::User => "user",
            Self::Ephemeral => "ephemeral",
        }
    }
}

/// The persisted document.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Document {
    schema: u32,
    install: String,
    salt: String,
    scope: Scope,
    epoch: u32,
    created_at_unix: u64,
}

impl Document {
    fn fresh(scope: Scope, epoch: u32) -> Self {
        Self {
            schema: SCHEMA,
            install: new_ulid(),
            salt: new_salt(),
            scope,
            epoch,
            created_at_unix: unix_now(),
        }
    }
}

/// The identity of this machine.
#[derive(Debug, Clone)]
pub struct MachineIdentity {
    document: Document,
    location: Option<PathBuf>,
}

impl MachineIdentity {
    /// Read the identity, or create one.
    ///
    /// Always returns a usable identity: a failure that did not prevent a
    /// fallback is returned alongside it so the caller can warn once. A caller
    /// must not treat the error as fatal — losing the identity file is a
    /// degraded state, not a reason to refuse to run.
    #[must_use]
    pub fn load_or_create() -> (Self, Option<Error>) {
        let mut first_error = None;
        for path in candidates() {
            match read(&path) {
                Ok(document) => {
                    return (
                        Self {
                            document,
                            location: Some(path),
                        },
                        first_error,
                    );
                }
                Err(error) if error.kind() == ErrorKind::NotFound => {}
                Err(error) => {
                    first_error.get_or_insert(error);
                }
            }
        }
        match create() {
            Ok(identity) => (identity, first_error),
            Err(error) => (Self::ephemeral(), Some(first_error.unwrap_or(error))),
        }
    }

    /// Replace the identity with a fresh one, keeping the location and bumping
    /// the epoch.
    ///
    /// Machine-scoped identities can only be reset by their owner: the call
    /// fails with [`ErrorKind::Forbidden`] for anyone else.
    pub fn reset() -> Result<Self> {
        let Some((path, existing)) = locate() else {
            return create();
        };
        reset_at(&path, existing.scope, existing.epoch)
    }

    /// The identifier itself.
    #[must_use]
    pub fn install(&self) -> &str {
        &self.document.install
    }

    /// Where the identity came from.
    #[must_use]
    pub fn scope(&self) -> Scope {
        self.document.scope
    }

    /// How many times the identity has been reset.
    #[must_use]
    pub fn epoch(&self) -> u32 {
        self.document.epoch
    }

    /// The file this identity lives in, when it is persisted.
    #[must_use]
    pub fn location(&self) -> Option<&Path> {
        self.location.as_deref()
    }

    /// A short form a user can read out to support.
    ///
    /// Eight Crockford base32 characters: short enough to dictate over the
    /// phone, wide enough that a support desk will not see a collision at any
    /// plausible install base.
    #[must_use]
    pub fn support_code(&self) -> String {
        self.document.install.chars().take(8).collect()
    }

    /// A stable marker for the local user, or `None` where the platform cannot
    /// see one.
    ///
    /// Two users of one shared machine must be distinguishable in the records,
    /// but their names must not travel. This hashes the effective uid with the
    /// identity's salt: stable across restarts, different between users, and not
    /// reversible without the salt, which never leaves the machine.
    #[must_use]
    pub fn principal(&self) -> Option<String> {
        #[cfg(unix)]
        {
            let uid = nix::unistd::Uid::effective().as_raw();
            let seed = format!("{}:{}:{}", self.document.salt, self.document.install, uid);
            Some(format!("p-{}", base32(fnv1a(seed.as_bytes()), 8)))
        }
        #[cfg(not(unix))]
        {
            None
        }
    }

    fn ephemeral() -> Self {
        Self {
            document: Document::fresh(Scope::Ephemeral, 0),
            location: None,
        }
    }
}

/// The paths to try, in order.
///
/// An explicit override wins and suppresses the rest: a deployment that pinned
/// the location must not silently fall back to a different one.
fn candidates() -> Vec<PathBuf> {
    if let Some(path) = std::env::var_os(PATH_OVERRIDE) {
        return vec![PathBuf::from(path)];
    }
    let mut paths = Vec::with_capacity(2);
    if let Some(path) = machine_path() {
        paths.push(path);
    }
    if let Some(path) = user_path() {
        paths.push(path);
    }
    paths
}

fn machine_path() -> Option<PathBuf> {
    #[cfg(target_os = "linux")]
    {
        Some(PathBuf::from("/var/lib/flowcraft").join(FILE_NAME))
    }
    #[cfg(target_os = "macos")]
    {
        Some(PathBuf::from("/Library/Application Support/FlowCraft").join(FILE_NAME))
    }
    #[cfg(target_os = "windows")]
    {
        std::env::var_os("ProgramData")
            .map(|base| PathBuf::from(base).join("FlowCraft").join(FILE_NAME))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        None
    }
}

fn user_path() -> Option<PathBuf> {
    #[cfg(target_os = "linux")]
    {
        let base = std::env::var_os("XDG_STATE_HOME")
            .map(PathBuf::from)
            .or_else(|| {
                std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".local/state"))
            })?;
        Some(base.join("flowcraft").join(FILE_NAME))
    }
    #[cfg(target_os = "macos")]
    {
        let home = std::env::var_os("HOME")?;
        Some(
            PathBuf::from(home)
                .join("Library/Application Support/FlowCraft")
                .join(FILE_NAME),
        )
    }
    #[cfg(target_os = "windows")]
    {
        std::env::var_os("LOCALAPPDATA")
            .map(|base| PathBuf::from(base).join("FlowCraft").join(FILE_NAME))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        None
    }
}

// Assembly: there is no executor to block while a capability is being built.
#[allow(clippy::disallowed_methods)]
fn read(path: &Path) -> Result<Document> {
    let bytes = std::fs::read(path)?;
    let document: Document = serde_json::from_slice(&bytes).map_err(|error| {
        Error::validation(format!(
            "identity: {} is not valid: {error}",
            path.display()
        ))
    })?;
    if document.schema != SCHEMA {
        return Err(Error::validation(format!(
            "identity: {} has schema {}, this build understands {SCHEMA}",
            path.display(),
            document.schema
        )));
    }
    Ok(document)
}

/// Create an identity at the first usable location.
fn create() -> Result<MachineIdentity> {
    let mut last = None;
    for path in candidates() {
        match create_at(&path) {
            Ok(identity) => return Ok(identity),
            Err(error) => last = Some(error),
        }
    }
    Err(last.unwrap_or_else(|| {
        Error::not_available("identity: no usable location for the install identity")
    }))
}

fn create_at(path: &Path) -> Result<MachineIdentity> {
    // An override is an operator statement ("this is the machine identity"), so
    // it is labelled like one rather than demoted to a per-user file.
    let overridden = std::env::var_os(PATH_OVERRIDE).is_some_and(|value| Path::new(&value) == path);
    let scope = if overridden || Some(path) == machine_path().as_deref() {
        Scope::Machine
    } else {
        Scope::User
    };
    create_at_with_epoch(path, scope, 0)
}

fn create_at_with_epoch(path: &Path, scope: Scope, epoch: u32) -> Result<MachineIdentity> {
    let document = Document::fresh(scope, epoch);
    match write_new(path, &document) {
        Ok(()) => Ok(MachineIdentity {
            document,
            location: Some(path.to_path_buf()),
        }),
        // Two daemons starting at once: the loser adopts the winner's file.
        Err(error) if error.kind() == ErrorKind::Conflict => Ok(MachineIdentity {
            document: read(path)?,
            location: Some(path.to_path_buf()),
        }),
        Err(error) => Err(error),
    }
}

/// Replace the document at `path`, bumping the epoch.
///
/// Split out from [`MachineIdentity::reset`] so the behaviour is testable
/// without touching process-wide environment state.
fn reset_at(path: &Path, scope: Scope, epoch: u32) -> Result<MachineIdentity> {
    std::fs::remove_file(path)?;
    create_at_with_epoch(path, scope, epoch.saturating_add(1))
}

fn write_new(path: &Path, document: &Document) -> Result<()> {
    if let Some(parent) = path.parent()
        && !parent.as_os_str().is_empty()
    {
        std::fs::create_dir_all(parent)?;
    }
    let bytes = serde_json::to_vec_pretty(document)
        .map_err(|error| Error::internal(format!("identity: cannot encode: {error}")))?;
    // `create_new` is the claim: whoever wins the race owns the file, and the
    // loser's `AlreadyExists` maps to `Conflict` without a second check.
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    set_readable(path)?;
    Ok(())
}

/// Machine identities are readable by every local user on purpose: that is the
/// whole point of "one per machine" with unprivileged daemons.
fn set_readable(path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o644))?;
    }
    #[cfg(not(unix))]
    {
        let _ = path;
    }
    Ok(())
}

/// The location and contents of the identity currently in force.
fn locate() -> Option<(PathBuf, Document)> {
    for path in candidates() {
        if let Ok(document) = read(&path) {
            return Some((path, document));
        }
    }
    None
}

fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs())
        .unwrap_or(0)
}

/// Crockford base32, the alphabet ULIDs use.
const CROCKFORD: &[u8; 32] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/// A ULID: 48 bits of millisecond timestamp, 80 bits of randomness.
///
/// Chosen over a UUID because it sorts by creation time, which makes a support
/// bundle readable without a decoder.
fn new_ulid() -> String {
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_millis() as u64)
        .unwrap_or(0);
    let mut out = String::with_capacity(26);
    // 48 bits do not divide by 5; the first character carries the top three.
    out.push(CROCKFORD[((millis >> 45) & 0x07) as usize] as char);
    for index in (0..9).rev() {
        out.push(CROCKFORD[((millis >> (index * 5)) & 0x1f) as usize] as char);
    }
    let mut random: u128 = 0;
    for _ in 0..5 {
        random = (random << 16) | u128::from(rand::random::<u16>());
    }
    for index in (0..16).rev() {
        out.push(CROCKFORD[((random >> (index * 5)) & 0x1f) as usize] as char);
    }
    out
}

fn new_salt() -> String {
    (0..16)
        .map(|_| format!("{:02x}", rand::random::<u8>()))
        .collect()
}

/// FNV-1a, stable across releases and platforms.
///
/// Unix-only: the principal it feeds is the one place a name is hashed.
///
/// The goal is not secrecy against an attacker holding the salt — it is that a
/// name never travels. A digest that changes with a dependency upgrade would
/// break correlation, which is worse than a non-cryptographic hash.
#[cfg(unix)]
fn fnv1a(bytes: &[u8]) -> u64 {
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for byte in bytes {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    hash
}

/// The low `width`×5 bits of `value`, Crockford base32.
#[cfg(unix)]
fn base32(value: u64, width: usize) -> String {
    let mut out = String::with_capacity(width);
    for index in (0..width).rev() {
        out.push(CROCKFORD[((value >> (index * 5)) & 0x1f) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ulids_are_well_formed_and_unique() {
        let first = new_ulid();
        let second = new_ulid();
        assert_eq!(first.len(), 26);
        assert!(first.bytes().all(|byte| CROCKFORD.contains(&byte)));
        assert_ne!(first, second);
        // The timestamp prefix is monotonic enough to sort two ids created in
        // the same millisecond only by their random tail, which is why the
        // leading character stays in range instead of overflowing.
        assert!((b'0'..=b'7').contains(&first.as_bytes()[0]));
    }

    #[test]
    fn a_created_identity_round_trips() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("identity.json");
        let created = create_at_with_epoch(&path, Scope::User, 0).expect("create");
        assert_eq!(created.scope(), Scope::User);
        assert_eq!(created.location(), Some(path.as_path()));

        assert_eq!(read(&path).expect("read").install, created.install());

        // A second creation at the same path adopts the first document instead
        // of overwriting it: two daemons starting together must agree.
        let adopted = create_at_with_epoch(&path, Scope::User, 0).expect("adopt");
        assert_eq!(adopted.install(), created.install());
    }

    #[test]
    fn reset_bumps_the_epoch_and_changes_the_id() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("identity.json");
        let created = create_at_with_epoch(&path, Scope::User, 0).expect("create");

        let reset = reset_at(&path, Scope::User, created.epoch()).expect("reset");
        assert_eq!(reset.epoch(), 1);
        assert_ne!(reset.install(), created.install());
        assert_eq!(read(&path).expect("read").epoch, 1);
    }

    #[test]
    fn support_code_is_the_leading_eight_characters() {
        let identity = MachineIdentity::ephemeral();
        assert_eq!(identity.support_code().len(), 8);
        assert!(identity.install().starts_with(&identity.support_code()));
    }

    #[test]
    fn principal_is_stable_for_one_identity() {
        let identity = MachineIdentity::ephemeral();
        assert_eq!(identity.principal(), identity.principal());
    }

    #[cfg(unix)]
    #[test]
    fn principal_differs_between_identities() {
        let first = MachineIdentity::ephemeral();
        let second = MachineIdentity::ephemeral();
        assert_ne!(first.principal(), second.principal());
    }

    #[test]
    fn the_salt_is_hex_and_not_reused() {
        let first = new_salt();
        assert_eq!(first.len(), 32);
        assert!(first.chars().all(|ch| ch.is_ascii_hexdigit()));
        assert_ne!(first, new_salt());
    }

    #[test]
    fn a_foreign_schema_is_rejected() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("identity.json");
        std::fs::write(
            &path,
            br#"{"schema":99,"install":"X","salt":"Y","scope":"user","epoch":0,"created_at_unix":0}"#,
        )
        .expect("write");
        let error = read(&path).expect_err("schema mismatch");
        assert_eq!(error.kind(), ErrorKind::Validation);
    }

    #[test]
    fn unknown_fields_are_rejected() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("identity.json");
        let document = Document::fresh(Scope::User, 0);
        let mut value = serde_json::to_value(&document).expect("encode");
        value["hostname"] = serde_json::json!("leaky");
        std::fs::write(&path, serde_json::to_vec(&value).expect("bytes")).expect("write");
        let error = read(&path).expect_err("unknown field");
        assert_eq!(error.kind(), ErrorKind::Validation);
    }
}
