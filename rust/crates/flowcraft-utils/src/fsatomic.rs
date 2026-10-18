//! `core/utils/fsatomic`: publish a file atomically.
//!
//! The payload goes to a temp file in the destination directory, optionally
//! fsynced, then renamed over the target, so a crash mid-write never leaves a
//! truncated file at the destination.
//!
//! It exists because the same temp-file dance kept being hand-rolled with
//! slightly different options at each call site. The options below are exactly
//! those differences, not a redesign: callers keep the mkdir/perm/sync behaviour
//! they had, and the failure paths (temp removed, one warning) live here once.
//!
//! # What the port does not carry yet
//!
//! core logs one warning when the *cleanup* removal fails after a failed write
//! (`telemetry.WarnErr`), because the write's own error is the one the caller
//! needs. There is no telemetry sink in the port yet, so the removal failure is
//! dropped rather than reported -- the same decision, with nowhere to say it.
//! The telemetry slice owns the sink, and this is the place to reconnect.

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

/// What the call sites actually differ in. The default is meaningful: no chmod,
/// no mkdir, no fsync, `.tmp-` prefix.
#[derive(Debug, Clone, Default)]
pub struct Options {
    /// The final permission of the published file. `None` keeps the temp file's
    /// own mode (0600).
    pub perm: Option<u32>,
    /// When set, creates the parent directory with this mode before writing.
    pub mkdir_perm: Option<u32>,
    /// The temp file's name prefix; `None` means `.tmp-`.
    pub temp_prefix: Option<String>,
    /// fsyncs the temp file before the rename and the destination directory
    /// after it, so the published name survives a power loss and not just the
    /// data. Windows cannot flush a directory handle; there the directory step
    /// is skipped.
    pub sync: bool,
}

/// A failure this module reports. The messages are core's, prefix for prefix,
/// because a caller matching on them matches the same sentences.
#[derive(Debug)]
#[non_exhaustive]
pub enum Error {
    /// The parent directory could not be created.
    CreateDirectory(io::Error),
    /// The temp file could not be created.
    CreateTemp(io::Error),
    /// The temp file could not be chmodded.
    Chmod(io::Error),
    /// The payload could not be written.
    Write(io::Error),
    /// The temp file could not be fsynced.
    Sync(io::Error),
    /// The temp file could not be closed.
    Close(io::Error),
    /// The temp file could not be renamed over the destination.
    Rename(io::Error),
    /// The directory could not be fsynced after the rename.
    SyncDirectory(io::Error),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let (what, cause) = match self {
            Error::CreateDirectory(cause) => ("create directory", cause),
            Error::CreateTemp(cause) => ("create temp file", cause),
            Error::Chmod(cause) => ("chmod temp file", cause),
            Error::Write(cause) => ("write temp file", cause),
            Error::Sync(cause) => ("sync temp file", cause),
            Error::Close(cause) => ("close temp file", cause),
            Error::Rename(cause) => ("rename temp file", cause),
            Error::SyncDirectory(cause) => ("sync directory", cause),
        };
        write!(f, "fsatomic: {what}: {cause}")
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        let cause = match self {
            Error::CreateDirectory(cause)
            | Error::CreateTemp(cause)
            | Error::Chmod(cause)
            | Error::Write(cause)
            | Error::Sync(cause)
            | Error::Close(cause)
            | Error::Rename(cause)
            | Error::SyncDirectory(cause) => cause,
        };
        Some(cause)
    }
}

/// Publish `data` at `path`: same-directory temp file, chmod, write, (sync),
/// close, rename.
///
/// On every failure path the temp file is removed and the destination is left
/// untouched.
pub fn write(path: impl AsRef<Path>, data: &[u8], options: &Options) -> Result<(), Error> {
    let path = path.as_ref();
    let dir = path.parent().unwrap_or(Path::new("."));
    if let Some(mode) = options.mkdir_perm {
        fs::create_dir_all(dir).map_err(Error::CreateDirectory)?;
        // create_dir_all takes the mode from the process umask; core's MkdirAll
        // takes it from the argument, so the last component is set explicitly.
        set_mode(dir, mode).map_err(Error::CreateDirectory)?;
    }
    let (temp, name) =
        create_temp(dir, options.temp_prefix.as_deref()).map_err(Error::CreateTemp)?;

    let outcome = publish(temp, &name, path, data, options);
    // No-op after a successful rename: the name no longer exists there.
    if fs::remove_file(&name).is_err() {
        // See the module docs: core logs this, and the port has no sink yet.
    }
    outcome
}

/// The steps that need the open temp file: chmod, write, sync, close, rename,
/// and the directory sync -- core's order.
///
/// Go checks the close's error and reports it; Rust's `File` has no fallible
/// close, because dropping it is the close. A close that fails after a
/// successful write and fsync is therefore not reportable here: it shows up on
/// the rename instead, or not at all. That is the one step of the sequence the
/// port cannot mirror, and the reason the order (close, then rename) is kept:
/// renaming a file that is still open is legal on Unix and not on Windows.
fn publish(
    mut temp: File,
    name: &Path,
    path: &Path,
    data: &[u8],
    options: &Options,
) -> Result<(), Error> {
    if let Some(mode) = options.perm {
        set_mode(name, mode).map_err(Error::Chmod)?;
    }
    temp.write_all(data).map_err(Error::Write)?;
    if options.sync {
        temp.sync_all().map_err(Error::Sync)?;
    }
    drop(temp);
    fs::rename(name, path).map_err(Error::Rename)?;
    if options.sync {
        sync_dir(path.parent().unwrap_or(Path::new("."))).map_err(Error::SyncDirectory)?;
    }
    Ok(())
}

/// `putenv`-free mode setting: the file is already open, so the path is what we
/// have; the standard library has no `fchmod`.
#[cfg(unix)]
fn set_mode(path: &Path, mode: u32) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))
}

#[cfg(not(unix))]
fn set_mode(_path: &Path, _mode: u32) -> io::Result<()> {
    Ok(())
}

/// Make the rename itself durable by flushing the directory entry, not just the
/// file contents. Windows cannot open a directory for that; core skips it, and
/// so does this.
fn sync_dir(dir: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        File::open(dir)?.sync_all()
    }
    #[cfg(not(unix))]
    {
        let _ = dir;
        Ok(())
    }
}

/// `os.CreateTemp` in `dir`: `O_CREAT | O_EXCL | O_RDWR`, mode 0600, a random
/// suffix, and the name returned so the failure paths can remove it.
///
/// The randomness is a counter and the clock rather than a generator: the
/// exclusive create is what makes the name safe, and a retry covers a collision
/// the way `mkstemp` does.
fn create_temp(dir: &Path, prefix: Option<&str>) -> io::Result<(File, PathBuf)> {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let prefix = prefix.unwrap_or(".tmp-");
    let mut last = None;
    for _ in 0..1000 {
        let attempt = COUNTER.fetch_add(1, Ordering::Relaxed);
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|elapsed| elapsed.subsec_nanos() as u64)
            .unwrap_or(0);
        let name = dir.join(format!(
            "{prefix}{:08x}{:08x}{:04x}",
            std::process::id(),
            nanos,
            attempt & 0xffff
        ));
        match OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(&name)
        {
            Ok(file) => {
                set_mode(&name, 0o600)?;
                return Ok((file, name));
            }
            Err(err) if err.kind() == io::ErrorKind::AlreadyExists => {
                last = Some(err);
                continue;
            }
            Err(err) => return Err(err),
        }
    }
    Err(last.unwrap_or_else(|| io::Error::other("fsatomic: no temp name available")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(label: &str) -> io::Result<PathBuf> {
        let dir = std::env::temp_dir().join(format!(
            "fsatomic-{label}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|elapsed| elapsed.subsec_nanos())
                .unwrap_or(0)
        ));
        std::fs::create_dir_all(&dir)?;
        Ok(dir)
    }

    fn entries(dir: &Path) -> io::Result<Vec<String>> {
        let mut names: Vec<String> = std::fs::read_dir(dir)?
            .filter_map(Result::ok)
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        Ok(names)
    }

    #[test]
    fn publishes_the_payload_and_leaves_no_temp() -> io::Result<()> {
        let dir = scratch("plain")?;
        let path = dir.join("state.json");
        write(&path, b"{\"a\":1}", &Options::default())
            .map_err(|err| io::Error::other(err.to_string()))?;
        assert_eq!(fs::read(&path)?, b"{\"a\":1}");
        assert_eq!(entries(&dir)?, ["state.json"], "a temp file survived");
        fs::remove_dir_all(&dir)?;
        Ok(())
    }

    #[test]
    fn creates_the_parent_directory_with_the_given_mode() -> io::Result<()> {
        let dir = scratch("nested")?;
        let path = dir.join("a/b/state.json");
        write(
            &path,
            b"x",
            &Options {
                mkdir_perm: Some(0o750),
                ..Options::default()
            },
        )
        .map_err(|err| io::Error::other(err.to_string()))?;
        assert_eq!(fs::read(&path)?, b"x");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(dir.join("a/b"))?.permissions().mode() & 0o777;
            assert_eq!(mode, 0o750);
        }
        fs::remove_dir_all(&dir)?;
        Ok(())
    }

    #[cfg(unix)]
    #[test]
    fn the_published_file_carries_the_permission() -> io::Result<()> {
        use std::os::unix::fs::PermissionsExt;
        let dir = scratch("perm")?;
        let path = dir.join("secret");
        write(
            &path,
            b"token",
            &Options {
                perm: Some(0o600),
                ..Options::default()
            },
        )
        .map_err(|err| io::Error::other(err.to_string()))?;
        let mode = fs::metadata(&path)?.permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        fs::remove_dir_all(&dir)?;
        Ok(())
    }

    #[test]
    fn sync_publishes_too() -> io::Result<()> {
        let dir = scratch("sync")?;
        let path = dir.join("durable");
        write(
            &path,
            b"durable bytes",
            &Options {
                sync: true,
                temp_prefix: Some(".custom-".to_owned()),
                ..Options::default()
            },
        )
        .map_err(|err| io::Error::other(err.to_string()))?;
        assert_eq!(fs::read(&path)?, b"durable bytes");
        assert_eq!(entries(&dir)?, ["durable"], "a temp file survived");
        fs::remove_dir_all(&dir)?;
        Ok(())
    }

    /// A failed publish leaves the destination exactly as it was, and takes its
    /// temp file with it. A rename onto a directory is the failure that is easy
    /// to reach on every platform.
    #[test]
    fn a_failed_rename_removes_the_temp_and_leaves_the_target() -> io::Result<()> {
        let dir = scratch("failed")?;
        let target = dir.join("taken");
        fs::create_dir_all(&target)?;
        let error = match write(&target, b"payload", &Options::default()) {
            Ok(()) => panic!("wrote over a directory"),
            Err(error) => error,
        };
        assert!(matches!(error, Error::Rename(_)), "{error}");
        assert!(
            error
                .to_string()
                .starts_with("fsatomic: rename temp file: ")
        );
        assert_eq!(
            entries(&dir)?,
            ["taken"],
            "a temp file survived the failure"
        );
        // The destination is still the directory it was.
        assert!(fs::metadata(&target)?.is_dir());
        fs::remove_dir_all(&dir)?;
        Ok(())
    }

    /// A second write replaces the first, which is what makes the rename the
    /// publication: there is never a window with no file at the destination.
    #[test]
    fn a_second_write_replaces_the_first() -> io::Result<()> {
        let dir = scratch("replace")?;
        let path = dir.join("state");
        for payload in [b"one".as_slice(), b"two".as_slice()] {
            write(&path, payload, &Options::default())
                .map_err(|err| io::Error::other(err.to_string()))?;
        }
        assert_eq!(fs::read(&path)?, b"two");
        assert_eq!(entries(&dir)?, ["state"]);
        fs::remove_dir_all(&dir)?;
        Ok(())
    }
}
