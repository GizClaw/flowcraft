//! The `keychain` store: an encrypted, file-based credential backend.
//!
//! One sealed file per secret under a `0700` directory, each sealed with
//! AES-256-GCM under a machine-local 32-byte key stored beside them as `.key`
//! (`0600`). The account name is the additional authenticated data, so a sealed
//! file moved onto another account's name fails to open instead of handing back
//! the wrong credential.
//!
//! This protects against accidental exposure — backups, file sharing, casual
//! reads — not against another process running as the same user. A true OS
//! credential store (Keychain, libsecret, DPAPI) would be a different backend.
//!
//! Deployment form:
//!
//! ```yaml
//! resources:
//!   secrets:
//!     kind: secret.Store
//!     impl: keychain
//!     settings:
//!       dir: /var/lib/flowcraft/secrets
//!       id: keychain     # ${secret:keychain.NAME}
//!       default: true    # ${secret:NAME}
//! ```
//!
//! [`Manager`] is the app-side handle (settings pages, CLIs) over the same
//! directory: every handle opened over one directory in this process shares one
//! backend, and therefore one index lock.

use std::collections::BTreeMap;
use std::fs::{self, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock, RwLock};

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use ring::aead::{AES_256_GCM, Aad, LessSafeKey, NONCE_LEN, Nonce, UnboundKey};
use ring::digest::{SHA256, digest};
use ring::rand::{SecureRandom, SystemRandom};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Handle, KIND, Mutable, Store};

/// Prefix every sealed file carries. A file without it is rejected rather than
/// read as plaintext.
const MAGIC: &[u8] = b"ocenc1:";

/// AES-256 key size.
const KEY_LEN: usize = 32;

/// The machine-local key file inside the store directory.
const KEY_FILE: &str = ".key";

/// The account index: the only way to enumerate accounts, because file names
/// are hashes.
const ACCOUNTS_FILE: &str = "accounts.json";

/// How many times a reader waits for a concurrent first-run key write.
const KEY_READ_ATTEMPTS: usize = 100;

/// One wait step while a concurrent first run finishes writing `.key`.
const KEY_READ_STEP: std::time::Duration = std::time::Duration::from_millis(2);

/// The process-wide set of open stores.
static BACKENDS: OnceLock<Mutex<BTreeMap<StoreKey, Arc<FileBackend>>>> = OnceLock::new();

#[cfg(unix)]
type StoreKey = (u64, u64);
#[cfg(not(unix))]
type StoreKey = PathBuf;

fn backends() -> &'static Mutex<BTreeMap<StoreKey, Arc<FileBackend>>> {
    BACKENDS.get_or_init(|| Mutex::new(BTreeMap::new()))
}

#[cfg(unix)]
fn store_key(dir: &Path) -> Result<StoreKey> {
    use std::os::unix::fs::MetadataExt;
    let metadata = fs::metadata(dir)
        .map_err(|error| Error::internal(format!("secret keychain: inspect store dir: {error}")))?;
    Ok((metadata.dev(), metadata.ino()))
}

#[cfg(not(unix))]
fn store_key(dir: &Path) -> Result<StoreKey> {
    Ok(dir.to_path_buf())
}

/// The `keychain` store.
#[derive(Clone)]
pub struct Keychain {
    backend: Arc<FileBackend>,
    id: Option<String>,
    default: bool,
}

impl Keychain {
    /// Open (and create on first use) the store under `dir`.
    pub fn open(dir: impl AsRef<Path>) -> Result<Self> {
        Ok(Self {
            backend: FileBackend::open(dir.as_ref())?,
            id: None,
            default: false,
        })
    }

    /// Attach its deployment metadata.
    #[must_use]
    pub fn with_settings(mut self, id: Option<String>, default: bool) -> Self {
        self.id = id.filter(|id| !id.is_empty());
        self.default = default;
        self
    }

    /// The directory the store lives in.
    #[must_use]
    pub fn dir(&self) -> &Path {
        &self.backend.dir
    }
}

impl std::fmt::Debug for Keychain {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("keychain.Store")
            .field("dir", &self.backend.dir)
            .field("id", &self.id)
            .field("default", &self.default)
            .finish_non_exhaustive()
    }
}

#[async_trait]
impl Store for Keychain {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        self.backend.get(name)
    }

    fn mutable(&self) -> Option<&dyn Mutable> {
        Some(self)
    }
}

#[async_trait]
impl Mutable for Keychain {
    async fn set(&self, name: &str, value: &str) -> Result<()> {
        self.backend.set(name, value.as_bytes())
    }

    async fn delete(&self, name: &str) -> Result<()> {
        self.backend.delete(name)
    }

    async fn delete_prefix(&self, prefix: &str) -> Result<()> {
        self.backend.delete_prefix(prefix)
    }

    fn available(&self) -> bool {
        self.backend.available()
    }
}

/// The app-side credential handle.
///
/// Construction never fails: an unusable directory surfaces through
/// [`Manager::available`] and per-call errors, so a settings UI degrades instead
/// of refusing to open.
#[derive(Debug, Default)]
pub struct Manager {
    store: Option<Keychain>,
}

impl Manager {
    /// Open the store under `dir`, or an unavailable handle when it cannot.
    #[must_use]
    pub fn open(dir: impl AsRef<Path>) -> Self {
        Self {
            store: Keychain::open(dir).ok(),
        }
    }

    /// Wrap an already-open store.
    #[must_use]
    pub fn new(store: Keychain) -> Self {
        Self { store: Some(store) }
    }

    /// Whether the backend can serve reads and writes.
    #[must_use]
    pub fn available(&self) -> bool {
        self.store
            .as_ref()
            .is_some_and(|store| store.backend.available())
    }

    /// The store directory, when the handle is usable.
    #[must_use]
    pub fn dir(&self) -> Option<&Path> {
        self.store.as_ref().map(Keychain::dir)
    }

    /// Read one secret.
    pub async fn get(&self, name: &str) -> Result<Option<String>> {
        self.store()?.lookup(name).await
    }

    /// Write one secret.
    pub async fn set(&self, name: &str, value: &str) -> Result<()> {
        self.store()?.set(name, value).await
    }

    /// Remove one secret. A missing name is not an error.
    pub async fn delete(&self, name: &str) -> Result<()> {
        self.store()?.delete(name).await
    }

    /// Remove every secret under a prefix (an empty prefix is refused).
    pub async fn delete_prefix(&self, prefix: &str) -> Result<()> {
        self.store()?.delete_prefix(prefix).await
    }

    fn store(&self) -> Result<&Keychain> {
        self.store
            .as_ref()
            .ok_or_else(|| Error::not_available("secret keychain: store directory is unusable"))
    }
}

/// The storage behind a [`Keychain`].
///
/// `key` is replaced when the directory is reopened after its key file changed,
/// so an operation holds a read guard from the moment it takes the key until the
/// file sealed under that key has been published. Without that, a reopen
/// mid-flight could leave a file this process can no longer open.
struct FileBackend {
    dir: PathBuf,
    key: RwLock<[u8; KEY_LEN]>,
    /// Guards the accounts index read-modify-write.
    index: Mutex<()>,
}

impl std::fmt::Debug for FileBackend {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FileBackend")
            .field("dir", &self.dir)
            .finish_non_exhaustive()
    }
}

impl FileBackend {
    /// Open, or join, the backend for `dir`.
    fn open(dir: &Path) -> Result<Arc<Self>> {
        let backend = Self::load(dir)?;
        let key_id = store_key(&backend.dir)?;
        let mut registry = backends()
            .lock()
            .map_err(|_| Error::internal("secret keychain: store registry lock is poisoned"))?;
        if let Some(existing) = registry.get(&key_id) {
            // The key file on disk is the source of truth: a store that was
            // reset, restored, or re-keyed while this process ran must not go on
            // sealing with the key it remembers.
            let key = *backend.pinned_key()?;
            existing.set_key(key)?;
            return Ok(existing.clone());
        }
        registry.insert(key_id, backend.clone());
        Ok(backend)
    }

    /// Open the directory without joining the process-wide registry.
    ///
    /// Production always goes through [`Self::open`]; this is the "another
    /// process opens the same directory" path, which tests also use.
    fn load(dir: &Path) -> Result<Arc<Self>> {
        let dir = prepare_dir(dir)?;
        let key = load_or_create_key(&dir)?;
        Ok(Arc::new(Self {
            dir,
            key: RwLock::new(key),
            index: Mutex::new(()),
        }))
    }

    fn set_key(&self, key: [u8; KEY_LEN]) -> Result<()> {
        let mut guard = self
            .key
            .write()
            .map_err(|_| Error::internal("secret keychain: key lock is poisoned"))?;
        *guard = key;
        Ok(())
    }

    fn available(&self) -> bool {
        !self.dir.as_os_str().is_empty() && self.key.read().is_ok()
    }

    /// Pin the key for one operation. The guard is held across the I/O the key
    /// protects.
    fn pinned_key(&self) -> Result<std::sync::RwLockReadGuard<'_, [u8; KEY_LEN]>> {
        self.key
            .read()
            .map_err(|_| Error::internal("secret keychain: key lock is poisoned"))
    }

    fn path(&self, name: &str) -> PathBuf {
        let sum = digest(&SHA256, name.as_bytes());
        self.dir.join(hex(sum.as_ref()))
    }

    fn get(&self, name: &str) -> Result<Option<String>> {
        validate_name(name)?;
        let path = self.path(name);
        let raw = match fs::read(&path) {
            Ok(raw) => raw,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret keychain: read {name:?}: {error}"
                )));
            }
        };
        if !raw.starts_with(MAGIC) {
            // Unencrypted files are rejected, not silently read: the secret must
            // be re-entered so it is never left (or treated) as plaintext.
            return Err(Error::validation(format!(
                "secret keychain: {name:?} is not encrypted; re-enter the secret"
            )));
        }
        let key = self.pinned_key()?;
        let plain = open(&key, name, &raw)?;
        Ok(Some(String::from_utf8(plain).map_err(|_| {
            Error::internal(format!("secret keychain: {name:?} is not UTF-8"))
        })?))
    }

    fn set(&self, name: &str, value: &[u8]) -> Result<()> {
        validate_name(name)?;
        let key = self.pinned_key()?;
        let sealed = seal(&key, name, value)?;
        atomic_write(&self.dir, &self.path(name), &sealed, ".sealed")?;
        // The pin is released after the write, not after the encryption: see
        // `pinned_key`.
        drop(key);
        self.add_account(name)
    }

    fn delete(&self, name: &str) -> Result<()> {
        validate_name(name)?;
        match fs::remove_file(self.path(name)) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret keychain: delete {name:?}: {error}"
                )));
            }
        }
        self.remove_account(name)
    }

    fn delete_prefix(&self, prefix: &str) -> Result<()> {
        if prefix.trim().is_empty() {
            return Err(Error::validation(
                "secret keychain: refusing to delete every secret with an empty prefix",
            ));
        }
        let _index = self
            .index
            .lock()
            .map_err(|_| Error::internal("secret keychain: index lock is poisoned"))?;
        let accounts = self.read_accounts()?;
        let mut kept = Vec::with_capacity(accounts.len());
        for name in accounts {
            if name.starts_with(prefix) {
                match fs::remove_file(self.path(&name)) {
                    Ok(()) => {}
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                    Err(error) => {
                        return Err(Error::internal(format!(
                            "secret keychain: delete {name:?}: {error}"
                        )));
                    }
                }
            } else {
                kept.push(name);
            }
        }
        self.write_accounts(&kept)
    }

    fn add_account(&self, name: &str) -> Result<()> {
        let _index = self
            .index
            .lock()
            .map_err(|_| Error::internal("secret keychain: index lock is poisoned"))?;
        let mut accounts = self.read_accounts()?;
        if accounts.iter().any(|known| known == name) {
            return Ok(());
        }
        accounts.push(name.to_owned());
        self.write_accounts(&accounts)
    }

    fn remove_account(&self, name: &str) -> Result<()> {
        let _index = self
            .index
            .lock()
            .map_err(|_| Error::internal("secret keychain: index lock is poisoned"))?;
        let mut accounts = self.read_accounts()?;
        accounts.retain(|known| known != name);
        self.write_accounts(&accounts)
    }

    /// A missing index is a fresh store. An index that exists but cannot be read
    /// or decoded is an error, never an empty list: rewriting from empty would
    /// orphan every secret it still names.
    fn read_accounts(&self) -> Result<Vec<String>> {
        let path = self.dir.join(ACCOUNTS_FILE);
        let raw = match fs::read(&path) {
            Ok(raw) => raw,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret keychain: read account list: {error}"
                )));
            }
        };
        serde_json::from_slice(&raw).map_err(|error| {
            Error::internal(format!(
                "secret keychain: account list is undecodable, refusing to rewrite it: {error}"
            ))
        })
    }

    fn write_accounts(&self, accounts: &[String]) -> Result<()> {
        let raw = serde_json::to_vec(accounts).map_err(|error| {
            Error::internal(format!("secret keychain: encode accounts: {error}"))
        })?;
        atomic_write(&self.dir, &self.dir.join(ACCOUNTS_FILE), &raw, ".accounts")
    }
}

fn validate_name(name: &str) -> Result<()> {
    if name.trim().is_empty() {
        return Err(Error::validation("secret keychain: secret name is empty"));
    }
    Ok(())
}

/// Resolve, create, and secure the store directory.
fn prepare_dir(dir: &Path) -> Result<PathBuf> {
    if dir.as_os_str().is_empty() {
        return Err(Error::validation(
            "secret keychain: settings.dir is required",
        ));
    }
    let absolute = if dir.is_absolute() {
        dir.to_path_buf()
    } else {
        std::env::current_dir()
            .map_err(|error| Error::internal(format!("secret keychain: resolve cwd: {error}")))?
            .join(dir)
    };
    fs::create_dir_all(&absolute).map_err(|error| {
        Error::validation(format!(
            "secret keychain: create store dir {}: {error}",
            absolute.display()
        ))
    })?;
    // The directory a store is keyed by is the one the filesystem ends up at,
    // not the spelling the document used: a deployment that reaches it through a
    // symlink would otherwise be a second store with a second index lock over
    // the same files.
    let real = fs::canonicalize(&absolute).map_err(|error| {
        Error::validation(format!(
            "secret keychain: resolve store dir {}: {error}",
            absolute.display()
        ))
    })?;
    set_mode(&real, 0o700)?;
    if !fs::metadata(&real)
        .map_err(|error| Error::internal(format!("secret keychain: stat store dir: {error}")))?
        .is_dir()
    {
        return Err(Error::validation(format!(
            "secret keychain: {} is not a directory",
            real.display()
        )));
    }
    Ok(real)
}

#[cfg(unix)]
fn set_mode(path: &Path, mode: u32) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode)).map_err(|error| {
        Error::internal(format!(
            "secret keychain: set mode {mode:o} on {}: {error}",
            path.display()
        ))
    })
}

#[cfg(not(unix))]
fn set_mode(_path: &Path, _mode: u32) -> Result<()> {
    Ok(())
}

/// Publish `data` at `path` atomically.
///
/// A crash mid-write must not leave a truncated sealed file (GCM would fail
/// every later read) or a truncated index, so the bytes land in a temp file in
/// the same directory and are renamed over the target.
fn atomic_write(dir: &Path, path: &Path, data: &[u8], prefix: &str) -> Result<()> {
    // A random suffix keeps two processes (or a leftover file from a crash) from
    // claiming the same temp name; `create_new` is what makes the claim atomic.
    let mut attempt = 0;
    let (temp, mut file) = loop {
        let temp = dir.join(format!("{prefix}-{}.tmp", random_hex(8)?));
        match open_private_write(&temp) {
            Ok(file) => break (temp, file),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists && attempt < 8 => {
                attempt += 1;
            }
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret keychain: create temp file: {error}"
                )));
            }
        }
    };
    let written = file
        .write_all(data)
        .and_then(|()| file.sync_all())
        .map_err(|error| Error::internal(format!("secret keychain: write temp file: {error}")));
    drop(file);
    if let Err(error) = written {
        let _ = fs::remove_file(&temp);
        return Err(error);
    }
    fs::rename(&temp, path).map_err(|error| {
        let _ = fs::remove_file(&temp);
        Error::internal(format!(
            "secret keychain: publish {}: {error}",
            path.display()
        ))
    })
}

#[cfg(unix)]
fn open_private_write(path: &Path) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt;
    OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
}

#[cfg(not(unix))]
fn open_private_write(path: &Path) -> std::io::Result<std::fs::File> {
    OpenOptions::new().write(true).create_new(true).open(path)
}

/// Read the 32-byte key, minting it on first use.
///
/// Creation is claimed with `O_EXCL`, so a concurrent first run has exactly one
/// winner; the loser waits out the winner's write instead of reading a
/// half-written key. A store that already holds sealed secrets never mints a
/// key: it fails closed.
fn load_or_create_key(dir: &Path) -> Result<[u8; KEY_LEN]> {
    let path = dir.join(KEY_FILE);
    match fs::read(&path) {
        Ok(raw) => match decode_key(&raw) {
            Ok(key) => Ok(key),
            // The file exists but does not decode yet: a concurrent first run may
            // still be writing it (`O_EXCL` claims the name before the bytes
            // land). Wait for that writer; a genuinely truncated key file still
            // fails closed.
            Err(_) => read_key_retrying(&path),
        },
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            ensure_key_mintable(dir)?;
            mint_key(&path)
        }
        Err(error) => Err(Error::internal(format!(
            "secret keychain: read key file: {error}"
        ))),
    }
}

fn mint_key(path: &Path) -> Result<[u8; KEY_LEN]> {
    let mut key = [0u8; KEY_LEN];
    SystemRandom::new()
        .fill(&mut key)
        .map_err(|_| Error::internal("secret keychain: generate key"))?;
    let mut file = match open_private_write(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
            return read_key_retrying(path);
        }
        Err(error) => {
            return Err(Error::internal(format!(
                "secret keychain: create key file: {error}"
            )));
        }
    };
    let mut text = hex(&key);
    text.push('\n');
    let written = file
        .write_all(text.as_bytes())
        .and_then(|()| file.sync_all())
        .map_err(|error| Error::internal(format!("secret keychain: write key file: {error}")));
    drop(file);
    if let Err(error) = written {
        // A failure past this point removes the claimed file again, so a later
        // run can mint cleanly instead of tripping over a key file that will
        // never decode.
        let _ = fs::remove_file(path);
        return Err(error);
    }
    Ok(key)
}

/// Read a key a concurrent first run may still be writing.
fn read_key_retrying(path: &Path) -> Result<[u8; KEY_LEN]> {
    let mut last = String::from("key file is unreadable");
    for _ in 0..KEY_READ_ATTEMPTS {
        match fs::read(path) {
            Ok(raw) => match decode_key(&raw) {
                Ok(key) => return Ok(key),
                Err(error) => last = error.to_string(),
            },
            Err(error) => last = error.to_string(),
        }
        std::thread::sleep(KEY_READ_STEP);
    }
    Err(Error::internal(format!(
        "secret keychain: key file did not become usable: {last}"
    )))
}

/// Refuse to mint a fresh key when the directory already holds store data.
///
/// Anything else in the directory is not the store's to judge.
fn ensure_key_mintable(dir: &Path) -> Result<()> {
    let error = || {
        Error::validation(
            "secret keychain: the store holds sealed secrets but its key file is missing; restore it from backup instead of starting fresh",
        )
    };
    let entries = fs::read_dir(dir)
        .map_err(|error| Error::internal(format!("secret keychain: list store dir: {error}")))?;
    for entry in entries {
        let entry = entry.map_err(|error| {
            Error::internal(format!("secret keychain: list store dir: {error}"))
        })?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if name == KEY_FILE || entry.path().is_dir() {
            continue;
        }
        if name == ACCOUNTS_FILE {
            return Err(error());
        }
        // Only the magic prefix is needed: reading a whole secret file to decide
        // whether a key may be minted would be wasted I/O.
        let mut head = [0u8; MAGIC.len()];
        if let Ok(mut file) = fs::File::open(entry.path())
            && file.read(&mut head).unwrap_or(0) == MAGIC.len()
            && head.as_slice() == MAGIC
        {
            return Err(error());
        }
    }
    Ok(())
}

fn decode_key(raw: &[u8]) -> Result<[u8; KEY_LEN]> {
    let text = std::str::from_utf8(raw)
        .map_err(|_| Error::validation("secret keychain: key file is not text"))?;
    let bytes = unhex(text.trim())
        .ok_or_else(|| Error::validation("secret keychain: key file is not hexadecimal"))?;
    if bytes.len() != KEY_LEN {
        return Err(Error::validation(format!(
            "secret keychain: key file must hold {KEY_LEN} bytes, got {}",
            bytes.len()
        )));
    }
    let mut key = [0u8; KEY_LEN];
    key.copy_from_slice(&bytes);
    Ok(key)
}

fn seal(key: &[u8; KEY_LEN], name: &str, plain: &[u8]) -> Result<Vec<u8>> {
    let sealing = LessSafeKey::new(
        UnboundKey::new(&AES_256_GCM, key)
            .map_err(|_| Error::internal("secret keychain: invalid key"))?,
    );
    let mut nonce_bytes = [0u8; NONCE_LEN];
    SystemRandom::new()
        .fill(&mut nonce_bytes)
        .map_err(|_| Error::internal("secret keychain: generate nonce"))?;
    let nonce = Nonce::assume_unique_for_key(nonce_bytes);
    let mut out = Vec::with_capacity(MAGIC.len() + NONCE_LEN + plain.len() + 16);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&nonce_bytes);
    let mut body = plain.to_vec();
    sealing
        .seal_in_place_append_tag(nonce, Aad::from(name.as_bytes()), &mut body)
        .map_err(|_| Error::internal("secret keychain: encrypt"))?;
    out.extend_from_slice(&body);
    Ok(out)
}

fn open(key: &[u8; KEY_LEN], name: &str, raw: &[u8]) -> Result<Vec<u8>> {
    let body = raw
        .strip_prefix(MAGIC)
        .ok_or_else(|| Error::validation("secret keychain: missing seal prefix"))?;
    if body.len() < NONCE_LEN {
        return Err(Error::validation(
            "secret keychain: truncated sealed secret",
        ));
    }
    let (nonce_bytes, sealed) = body.split_at(NONCE_LEN);
    let mut raw_nonce = [0u8; NONCE_LEN];
    raw_nonce.copy_from_slice(nonce_bytes);
    let opening = LessSafeKey::new(
        UnboundKey::new(&AES_256_GCM, key)
            .map_err(|_| Error::internal("secret keychain: invalid key"))?,
    );
    let mut buffer = sealed.to_vec();
    if let Ok(plain) = opening.open_in_place(
        Nonce::assume_unique_for_key(raw_nonce),
        Aad::from(name.as_bytes()),
        &mut buffer,
    ) {
        return Ok(plain.to_vec());
    }
    // Files sealed before the name binding existed carry no additional data.
    // They are accepted so an existing store keeps working, and the next write
    // rewrites them bound.
    let mut buffer = sealed.to_vec();
    if let Ok(plain) = opening.open_in_place(
        Nonce::assume_unique_for_key(raw_nonce),
        Aad::empty(),
        &mut buffer,
    ) {
        return Ok(plain.to_vec());
    }
    Err(Error::internal(format!(
        "secret keychain: decrypt {name:?}: authentication failed"
    )))
}

fn random_hex(bytes: usize) -> Result<String> {
    let mut buffer = vec![0u8; bytes];
    SystemRandom::new()
        .fill(&mut buffer)
        .map_err(|_| Error::internal("secret keychain: generate random bytes"))?;
    Ok(hex(&buffer))
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(DIGITS[(byte >> 4) as usize] as char);
        out.push(DIGITS[(byte & 0x0f) as usize] as char);
    }
    out
}

fn unhex(text: &str) -> Option<Vec<u8>> {
    if text.len() % 2 != 0 {
        return None;
    }
    let mut out = Vec::with_capacity(text.len() / 2);
    for pair in text.as_bytes().chunks(2) {
        let high = digit(pair[0])?;
        let low = digit(pair[1])?;
        out.push(high << 4 | low);
    }
    Some(out)
}

fn digit(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    dir: String,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    default: bool,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    if settings.dir.trim().is_empty() {
        return Err(Error::validation("settings.dir is required"));
    }
    Ok(())
}

/// The `secret.Store/keychain` factory.
pub struct KeychainFactory;

impl Factory for KeychainFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("keychain"),
            deps: Vec::new(),
            phase: Phase::Pre,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let mut dir = PathBuf::from(&settings.dir);
            if dir.is_relative()
                && let Some(base) = ctx.base_dir
            {
                dir = base.join(dir);
            }
            let store = Keychain::open(&dir)?.with_settings(settings.id.clone(), settings.default);
            Ok(Arc::new(Handle::with_settings(
                Arc::new(store),
                settings.id,
                settings.default,
            )))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc as StdArc;

    fn dir() -> tempfile::TempDir {
        tempfile::tempdir().expect("tempdir")
    }

    fn store(root: &Path) -> Keychain {
        Keychain::open(root).expect("open")
    }

    fn mode(path: &Path) -> u32 {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::metadata(path).expect("metadata").permissions().mode() & 0o777
        }
        #[cfg(not(unix))]
        {
            let _ = path;
            0o600
        }
    }

    #[tokio::test]
    async fn set_get_and_delete_round_trip() {
        let root = dir();
        let store = store(root.path());
        assert_eq!(store.lookup("token").await.unwrap(), None);
        store.set("token", "s3cr3t").await.unwrap();
        assert_eq!(store.lookup("token").await.unwrap(), Some("s3cr3t".into()));
        // Overwrite.
        store.set("token", "rotated").await.unwrap();
        assert_eq!(store.lookup("token").await.unwrap(), Some("rotated".into()));
        store.delete("token").await.unwrap();
        assert_eq!(store.lookup("token").await.unwrap(), None);
        // Deleting a missing name is not an error.
        store.delete("token").await.unwrap();
    }

    #[tokio::test]
    async fn names_with_path_characters_cannot_escape_the_directory() {
        let root = dir();
        let store = store(root.path());
        store.set("../../etc/passwd", "x").await.unwrap();
        // Everything stays inside the store directory.
        let entries = fs::read_dir(root.path()).unwrap().count();
        assert_eq!(entries, 3, "sealed file, accounts index, and key file");
        assert_eq!(
            store.lookup("../../etc/passwd").await.unwrap(),
            Some("x".into())
        );
        assert!(store.set("  ", "x").await.is_err());
    }

    #[tokio::test]
    async fn the_file_on_disk_is_sealed_and_private() {
        let root = dir();
        let store = store(root.path());
        store.set("token", "hunter2").await.unwrap();
        let sealed = fs::read(store.backend.path("token")).unwrap();
        assert!(sealed.starts_with(MAGIC));
        assert!(
            !sealed
                .windows(b"hunter2".len())
                .any(|window| window == b"hunter2".as_slice()),
            "the plaintext must not be on disk"
        );
        assert_eq!(mode(&store.backend.path("token")), 0o600);
        assert_eq!(mode(root.path()), 0o700);
        assert_eq!(mode(&root.path().join(KEY_FILE)), 0o600);
        let key = fs::read_to_string(root.path().join(KEY_FILE)).unwrap();
        assert_eq!(key.trim().len(), KEY_LEN * 2, "hex-encoded 32-byte key");
    }

    #[tokio::test]
    async fn a_sealed_file_moved_onto_another_name_does_not_open() {
        let root = dir();
        let store = store(root.path());
        store.set("a", "value-of-a").await.unwrap();
        store.set("b", "value-of-b").await.unwrap();
        fs::rename(store.backend.path("a"), store.backend.path("b")).unwrap();
        let error = store.lookup("b").await.unwrap_err();
        assert!(error.to_string().contains("authentication"), "{error}");
    }

    #[tokio::test]
    async fn a_file_sealed_without_the_name_still_opens() {
        let root = dir();
        let store = store(root.path());
        let key = *store.backend.pinned_key().unwrap();
        // The pre-binding format: no additional data.
        let sealing = LessSafeKey::new(UnboundKey::new(&AES_256_GCM, &key).unwrap());
        let nonce_bytes = [7u8; NONCE_LEN];
        let mut body = b"legacy".to_vec();
        sealing
            .seal_in_place_append_tag(
                Nonce::assume_unique_for_key(nonce_bytes),
                Aad::empty(),
                &mut body,
            )
            .unwrap();
        let mut raw = MAGIC.to_vec();
        raw.extend_from_slice(&nonce_bytes);
        raw.extend_from_slice(&body);
        fs::write(store.backend.path("legacy"), raw).unwrap();
        assert_eq!(store.lookup("legacy").await.unwrap(), Some("legacy".into()));
    }

    #[tokio::test]
    async fn a_plaintext_file_is_rejected() {
        let root = dir();
        let store = store(root.path());
        fs::write(store.backend.path("plain"), b"not sealed").unwrap();
        let error = store.lookup("plain").await.unwrap_err();
        assert!(error.to_string().contains("not encrypted"), "{error}");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }

    #[tokio::test]
    async fn delete_prefix_removes_only_matches_and_refuses_empty() {
        let root = dir();
        let store = store(root.path());
        for name in ["team/a", "team/b", "other/c"] {
            store.set(name, "v").await.unwrap();
        }
        let error = store.delete_prefix("  ").await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
        store.delete_prefix("team/").await.unwrap();
        assert_eq!(store.lookup("team/a").await.unwrap(), None);
        assert_eq!(store.lookup("team/b").await.unwrap(), None);
        assert_eq!(store.lookup("other/c").await.unwrap(), Some("v".into()));
        let index: Vec<String> =
            serde_json::from_slice(&fs::read(root.path().join(ACCOUNTS_FILE)).unwrap()).unwrap();
        assert_eq!(index, vec!["other/c".to_owned()]);
    }

    #[tokio::test]
    async fn an_undecodable_index_is_an_error_not_an_empty_list() {
        let root = dir();
        let store = store(root.path());
        store.set("token", "v").await.unwrap();
        fs::write(root.path().join(ACCOUNTS_FILE), b"{not json").unwrap();
        // A broken index never hides a value: reads go to the sealed file only.
        assert_eq!(store.lookup("token").await.unwrap(), Some("v".into()));
        let error = store.set("other", "v").await.unwrap_err();
        assert!(error.to_string().contains("undecodable"), "{error}");
        assert!(store.delete("token").await.is_err());
        assert!(store.delete_prefix("t").await.is_err());
    }

    #[tokio::test]
    async fn a_missing_key_with_sealed_secrets_fails_closed() {
        let root = dir();
        let store = store(root.path());
        store.set("token", "v").await.unwrap();
        fs::remove_file(root.path().join(KEY_FILE)).unwrap();
        // A fresh backend (the "next process") must not mint a new key: every
        // sealed file would become unreadable.
        let error = FileBackend::load(root.path()).unwrap_err();
        assert!(error.to_string().contains("key file is missing"), "{error}");
    }

    #[test]
    fn a_missing_key_on_an_empty_directory_mints_one() {
        let root = dir();
        let first = FileBackend::load(root.path()).unwrap();
        assert!(root.path().join(KEY_FILE).exists());
        // Second open reads the same key back rather than minting another.
        let second = FileBackend::load(root.path()).unwrap();
        assert_eq!(*first.pinned_key().unwrap(), *second.pinned_key().unwrap());
    }

    #[test]
    fn a_truncated_key_file_fails_closed() {
        let root = dir();
        FileBackend::load(root.path()).unwrap();
        fs::write(root.path().join(KEY_FILE), b"abc").unwrap();
        let error = FileBackend::load(root.path()).unwrap_err();
        assert!(
            error.to_string().contains("did not become usable"),
            "{error}"
        );
    }

    #[test]
    fn one_directory_is_one_backend() {
        let root = dir();
        let first = FileBackend::open(root.path()).unwrap();
        let second = FileBackend::open(root.path()).unwrap();
        assert!(StdArc::ptr_eq(&first, &second));
        // ...also through a spelling the filesystem resolves to the same inode.
        #[cfg(unix)]
        {
            let linked = root.path().join("linked");
            std::os::unix::fs::symlink(root.path(), &linked).unwrap();
            let third = FileBackend::open(&linked).unwrap();
            assert!(StdArc::ptr_eq(&first, &third));
        }
    }

    #[tokio::test]
    async fn a_store_reopen_adopts_the_key_on_disk() {
        let root = dir();
        let store = store(root.path());
        store.set("token", "v").await.unwrap();
        // Re-key the directory behind the running store: the next reopen must
        // adopt the file, and the old sealed file must no longer open.
        let fresh = [9u8; KEY_LEN];
        fs::write(root.path().join(KEY_FILE), format!("{}\n", hex(&fresh))).unwrap();
        let reopened = FileBackend::open(root.path()).unwrap();
        assert_eq!(*reopened.pinned_key().unwrap(), fresh);
        assert!(reopened.get("token").is_err());
    }

    #[tokio::test]
    async fn the_manager_degrades_instead_of_failing() {
        let root = dir();
        let manager = Manager::open(root.path());
        assert!(manager.available());
        manager.set("token", "v").await.unwrap();
        assert_eq!(manager.get("token").await.unwrap(), Some("v".into()));

        let broken = Manager::open("/proc/definitely/not/here");
        assert!(!broken.available());
        assert_eq!(
            broken.get("x").await.unwrap_err().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[tokio::test]
    async fn the_handle_publishes_its_metadata_and_write_side() {
        let root = dir();
        let store = store(root.path()).with_settings(Some("team".into()), true);
        let handle = Handle::with_settings(Arc::new(store), Some("team".to_owned()), true);
        assert_eq!(handle.id(), Some("team"));
        assert!(handle.is_default());
        let mutable = handle.mutable().expect("keychain is mutable");
        mutable.set("k", "v").await.unwrap();
        assert_eq!(handle.lookup("k").await.unwrap(), Some("v".into()));
        assert!(mutable.available());
    }
}
