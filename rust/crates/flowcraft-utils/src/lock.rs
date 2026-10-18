//! `core/utils/lock`: one live holder per lock file, decided by the kernel.
//!
//! The advisory lock is `flock` (std's `File::try_lock`, which is `flock` on
//! Unix): a process that dies releases it, so a stale file never blocks the next
//! launch. What the file *contains* is the holder record -- pid, kind, start
//! time, version, endpoint -- which is what lets a rejected launch name the owner
//! instead of reporting a bare conflict. When the record and the kernel
//! disagree, the kernel wins.
//!
//! # The rules this keeps
//!
//! - Acquisition never waits: it takes the lock or reports [`Error`].
//! - A busy lock whose record names *this* pid is still busy, unless
//!   [`Options::share_within_process`] is set -- two components of one process
//!   do legitimately address one state root (an embedded host, tests), and they
//!   must not deadlock each other. The shared handle is `owned() == false` and
//!   its `release` is a no-op, because only the owner releases the kernel lock.
//! - The lock file stays behind after release, carrying the last holder's
//!   record: "who held this" stays answerable, and liveness is never inferred
//!   from it.
//!
//! # Differences from core
//!
//! - `build_version()` (`debug.ReadBuildInfo`) has no equivalent: a Rust binary
//!   does not carry a module version the runtime can read. The record's
//!   `version` is empty unless the caller passes one ([`Options::version`]).
//! - The clock formatting and parsing are a small civil-date conversion rather
//!   than a library: the record's `started` field is the one thing that needs a
//!   timestamp, and `live_from` parses only the shape this module writes
//!   (`YYYY-MM-DDTHH:MM:SSZ`). Go's `time.Parse(RFC3339)` also accepts fractional
//!   seconds and numeric offsets, which a record written by this module never
//!   has.

use std::fs::{File, OpenOptions};
use std::io::{self, Seek, Write};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

/// One lock holder. The holder writes it once the kernel lock is taken, which is
/// what lets a rejected launch name the owner of the file.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Info {
    /// The holder's process id.
    pub pid: u32,
    /// The surface holding the lock ("manager", "host", "gui").
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub kind: String,
    /// When the holder took the lock (RFC3339, UTC).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub started: String,
    /// The holder's build version, empty when the binary carries none.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version: String,
    /// An optional address at which the holder can be reached (to raise its
    /// window, to accept forwarded arguments). This module only records the
    /// value and hands it back with [`Error::Held`]; the protocol behind the
    /// address belongs to the caller.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub endpoint: String,
}

impl Info {
    /// Whether the holder recorded its own liveness window starting at or before
    /// `at`. A convenience for diagnostics: the authoritative liveness signal is
    /// the kernel lock, never this record.
    pub fn live_from(&self, at: SystemTime) -> bool {
        match parse_rfc3339_utc(&self.started) {
            Some(started) => started <= at,
            None => false,
        }
    }
}

/// What an [`acquire`] call can fail with.
#[derive(Debug)]
#[non_exhaustive]
pub enum Error {
    /// The lock path was empty or whitespace.
    EmptyPath,
    /// The lock's parent directory could not be created.
    CreateDirectory {
        /// The directory itself.
        path: PathBuf,
        /// What the filesystem said.
        source: io::Error,
    },
    /// The lock file could not be opened.
    Open {
        /// The lock file.
        path: PathBuf,
        /// What the filesystem said.
        source: io::Error,
    },
    /// The kernel lock could not be taken for a reason other than "busy".
    Lock {
        /// The lock file.
        path: PathBuf,
        /// What the kernel said.
        source: io::Error,
    },
    /// The lock was taken but the holder record could not be written.
    Record {
        /// The lock file.
        path: PathBuf,
        /// What the filesystem said.
        source: io::Error,
    },
    /// Another live holder owns the file. Refuse.
    ///
    /// Boxed because the record is the largest thing an error here can carry,
    /// and every `Result` in the module would otherwise be its size.
    Held(Box<Held>),
}

/// Another live holder owns the lock.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Held {
    /// The lock file.
    pub path: PathBuf,
    /// The holder as recorded in the file (zero values when the file holds no
    /// record this module can read).
    pub info: Info,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::EmptyPath => f.write_str("lock: lock path is empty"),
            Error::CreateDirectory { source, .. } => {
                write!(f, "lock: create lock dir: {source}")
            }
            Error::Open { source, .. } => write!(f, "lock: open lock file: {source}"),
            Error::Lock { path, source } => {
                write!(f, "lock: lock {}: {source}", path.display())
            }
            Error::Record { source, .. } => write!(f, "lock: record holder: {source}"),
            Error::Held(held) => held.fmt(f),
        }
    }
}

impl std::fmt::Display for Held {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.info.pid == 0 {
            return write!(
                f,
                "lock: {} is held by another live process",
                self.path.display()
            );
        }
        let mut who = format!("pid {}", self.info.pid);
        if !self.info.kind.is_empty() {
            who.push_str(" (");
            who.push_str(&self.info.kind);
            who.push(')');
        }
        if !self.info.started.is_empty() {
            who.push_str(" since ");
            who.push_str(&self.info.started);
        }
        write!(f, "lock: {} is held by {who}", self.path.display())
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::CreateDirectory { source, .. }
            | Error::Open { source, .. }
            | Error::Lock { source, .. }
            | Error::Record { source, .. } => Some(source),
            Error::EmptyPath | Error::Held(_) => None,
        }
    }
}

impl Error {
    /// Whether this failure means "another live holder", and which one. The
    /// counterpart of core's `IsHeld`: a caller that wraps `acquire`'s failure
    /// in its own conflict error still reaches the holder record through it.
    pub fn held(&self) -> Option<&Held> {
        match self {
            Error::Held(held) => Some(held),
            _ => None,
        }
    }
}

/// The resolved option set of one acquisition.
#[derive(Debug, Clone, Default)]
pub struct Options {
    /// Lets a second acquisition of one path inside one process succeed as a
    /// shared handle instead of reporting the file as held.
    ///
    /// The default is strict, which is what a single-instance lock wants: a
    /// second holder inside the same process is still a second instance.
    ///
    /// The same-process case is detected from the pid in the holder record, so
    /// two processes that report one pid (containers, pid namespaces) only ever
    /// get the shared answer, never a corrupted one.
    pub share_within_process: bool,
    /// The holder's build version ([`Info::version`]).
    pub version: String,
    /// An address a rejected launch can use to reach the holder.
    pub endpoint: String,
}

/// A held lock. Release it, or let the process die, to hand the file to the next
/// process: the kernel drops the lock either way.
#[derive(Debug)]
pub struct Handle {
    path: PathBuf,
    info: Info,
    file: Option<File>,
    shared: bool,
}

impl Handle {
    /// The lock file this handle holds.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// The holder this handle recorded -- its own process, unless [`owned`]
    /// reports false.
    ///
    /// [`owned`]: Handle::owned
    pub fn info(&self) -> &Info {
        &self.info
    }

    /// Whether this handle owns the kernel lock. It is false for a handle
    /// `share_within_process` handed to a second caller of the same process: the
    /// lock is effectively ours either way, but only the owning handle releases
    /// it.
    pub fn owned(&self) -> bool {
        !self.shared && self.file.is_some()
    }

    /// Drops the lock. The lock file stays behind with the last holder's record.
    /// Release on a shared handle is a no-op, and releasing twice is too.
    pub fn release(&mut self) -> io::Result<()> {
        let Some(file) = self.file.take() else {
            return Ok(());
        };
        let unlocked = file.unlock();
        // Dropping the handle closes the descriptor, which releases the lock
        // even when the explicit unlock failed.
        drop(file);
        unlocked
    }
}

/// Take the lock on `path` without blocking.
///
/// - `Ok(handle)`: this process holds the lock. A handle whose [`Handle::owned`]
///   reports false was handed to a second caller of this process under
///   [`Options::share_within_process`]; its release is a no-op.
/// - `Err(error)` with [`Error::held`] returning the holder: another live
///   holder. Refuse.
/// - `Err(error)` otherwise: the lock could not be taken, or the holder record
///   could not be written.
pub fn acquire(path: impl AsRef<Path>, kind: &str, options: &Options) -> Result<Handle, Error> {
    acquire_at(path.as_ref(), kind, options, SystemTime::now())
}

/// [`acquire`] with the clock passed in, for the tests that pin the record's
/// timestamp.
fn acquire_at(
    path: &Path,
    kind: &str,
    options: &Options,
    now: SystemTime,
) -> Result<Handle, Error> {
    if path.as_os_str().is_empty() || path.to_string_lossy().trim().is_empty() {
        return Err(Error::EmptyPath);
    }
    if let Some(dir) = path.parent()
        && !dir.as_os_str().is_empty()
    {
        create_dir_all_with(dir, 0o700).map_err(|source| Error::CreateDirectory {
            path: dir.to_path_buf(),
            source,
        })?;
    }
    let mut open = OpenOptions::new();
    open.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        // The mode applies only when the file is created, like `os.OpenFile`.
        open.mode(0o600);
    }
    let mut file = open.open(path).map_err(|source| Error::Open {
        path: path.to_path_buf(),
        source,
    })?;
    match file.try_lock() {
        Ok(()) => {}
        Err(std::fs::TryLockError::WouldBlock) => {
            let holder = read_info(path).unwrap_or_default();
            if holder.pid == std::process::id() && options.share_within_process {
                return Ok(Handle {
                    path: path.to_path_buf(),
                    info: holder,
                    file: None,
                    shared: true,
                });
            }
            return Err(Error::Held(Box::new(Held {
                path: path.to_path_buf(),
                info: holder,
            })));
        }
        Err(std::fs::TryLockError::Error(source)) => {
            return Err(Error::Lock {
                path: path.to_path_buf(),
                source,
            });
        }
    }
    let info = Info {
        pid: std::process::id(),
        kind: kind.to_owned(),
        started: format_rfc3339_utc(now),
        version: options.version.clone(),
        endpoint: options.endpoint.clone(),
    };
    write_info(&mut file, &info).map_err(|source| Error::Record {
        path: path.to_path_buf(),
        source,
    })?;
    Ok(Handle {
        path: path.to_path_buf(),
        info,
        file: Some(file),
        shared: false,
    })
}

/// `os.MkdirAll(dir, mode)`: the mode applies to every directory this creates,
/// and an existing one keeps its own.
fn create_dir_all_with(dir: &Path, mode: u32) -> io::Result<()> {
    let mut missing: Vec<PathBuf> = Vec::new();
    let mut current = dir;
    while !current.exists() {
        missing.push(current.to_path_buf());
        match current.parent() {
            Some(parent) if !parent.as_os_str().is_empty() => current = parent,
            _ => break,
        }
    }
    for path in missing.iter().rev() {
        std::fs::create_dir(path)?;
        set_mode(path, mode)?;
    }
    Ok(())
}

/// The last holder recorded in a lock file, if any.
///
/// It reads the file only: the result says nothing about liveness, and a record
/// in a format this module does not know (an older release left a bare pid line)
/// reports nothing while the lock stays acquirable.
pub fn read_info(path: impl AsRef<Path>) -> Option<Info> {
    let raw = std::fs::read(path).ok()?;
    let info: Info = serde_json::from_slice(&raw).ok()?;
    (info.pid != 0).then_some(info)
}

/// Record the holder so the next process that finds the lock busy can name it.
fn write_info(file: &mut File, info: &Info) -> io::Result<()> {
    let mut raw = serde_json::to_vec(info).map_err(io::Error::other)?;
    raw.push(b'\n');
    file.set_len(0)?;
    file.rewind()?;
    file.write_all(&raw)?;
    file.sync_all()
}

#[cfg(unix)]
fn set_mode(path: &Path, mode: u32) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
}

#[cfg(not(unix))]
fn set_mode(_path: &Path, _mode: u32) -> io::Result<()> {
    Ok(())
}

/// `time.Time`'s RFC3339 in UTC, truncated to the second: what the record says.
pub fn format_rfc3339_utc(at: SystemTime) -> String {
    let seconds = at
        .duration_since(UNIX_EPOCH)
        .map(|elapsed| elapsed.as_secs() as i64)
        .unwrap_or(0);
    let (year, month, day) = civil_from_days(seconds.div_euclid(86_400));
    let rest = seconds.rem_euclid(86_400);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        rest / 3600,
        (rest % 3600) / 60,
        rest % 60
    )
}

/// Parse the shape [`format_rfc3339_utc`] writes, and nothing else.
fn parse_rfc3339_utc(text: &str) -> Option<SystemTime> {
    let bytes = text.as_bytes();
    if bytes.len() != 20 || bytes[4] != b'-' || bytes[7] != b'-' || bytes[10] != b'T' {
        return None;
    }
    if bytes[13] != b':' || bytes[16] != b':' || bytes[19] != b'Z' {
        return None;
    }
    let number = |from: usize, to: usize| -> Option<u32> {
        let field = &text[from..to];
        if !field.bytes().all(|byte| byte.is_ascii_digit()) {
            return None;
        }
        field.parse().ok()
    };
    let (year, month, day) = (number(0, 4)?, number(5, 7)?, number(8, 10)?);
    let (hour, minute, second) = (number(11, 13)?, number(14, 16)?, number(17, 19)?);
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }
    if hour > 23 || minute > 59 || second > 60 {
        return None;
    }
    let days = days_from_civil(i64::from(year), month, day);
    let seconds =
        days * 86_400 + i64::from(hour) * 3600 + i64::from(minute) * 60 + i64::from(second);
    Some(UNIX_EPOCH + Duration::from_secs(u64::try_from(seconds).ok()?))
}

/// The civil date for a day count since 1970-01-01 (Howard Hinnant's
/// `civil_from_days`).
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let shifted = days + 719_468;
    let era = shifted.div_euclid(146_097);
    let day_of_era = shifted.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let year = year_of_era + era * 400;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_prime = (5 * day_of_year + 2) / 153;
    let day = (day_of_year - (153 * month_prime + 2) / 5 + 1) as u32;
    let month = if month_prime < 10 {
        month_prime + 3
    } else {
        month_prime - 9
    } as u32;
    (year + i64::from(month <= 2), month, day)
}

/// The inverse of [`civil_from_days`].
fn days_from_civil(year: i64, month: u32, day: u32) -> i64 {
    let year = year - i64::from(month <= 2);
    let era = year.div_euclid(400);
    let year_of_era = year - era * 400;
    let month_prime = i64::from(if month > 2 { month - 3 } else { month + 9 });
    let day_of_year = (153 * month_prime + 2) / 5 + i64::from(day) - 1;
    let day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year;
    era * 146_097 + day_of_era - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(label: &str) -> io::Result<PathBuf> {
        let dir = std::env::temp_dir().join(format!(
            "lock-{label}-{}-{:?}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|elapsed| elapsed.subsec_nanos())
                .unwrap_or(0)
        ));
        std::fs::create_dir_all(&dir)?;
        Ok(dir)
    }

    #[test]
    fn a_second_acquisition_reports_the_holder() -> io::Result<()> {
        let dir = scratch("held")?;
        let path = dir.join("state.lock");
        let mut first = match acquire(&path, "manager", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert!(first.owned());
        assert_eq!(first.info().pid, std::process::id());
        assert_eq!(first.info().kind, "manager");

        let error = match acquire(&path, "host", &Options::default()) {
            Ok(handle) => panic!("a second acquisition succeeded: {handle:?}"),
            Err(error) => error,
        };
        let held = match error.held() {
            Some(held) => held.clone(),
            None => panic!("not a held error: {error}"),
        };
        assert_eq!(held.path, path);
        assert_eq!(held.info.pid, std::process::id());
        assert_eq!(held.info.kind, "manager");
        assert!(
            error.to_string().starts_with(&format!(
                "lock: {} is held by pid {} (manager) since ",
                path.display(),
                std::process::id()
            )),
            "{error}"
        );

        first.release()?;
        // The file keeps the last record, and the lock is free again.
        assert_eq!(
            read_info(&path).map(|info| info.kind),
            Some("manager".to_owned())
        );
        let second = match acquire(&path, "host", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert!(second.owned());
        Ok(())
    }

    #[test]
    fn sharing_within_the_process_hands_out_a_quiet_handle() -> io::Result<()> {
        let dir = scratch("share")?;
        let path = dir.join("state.lock");
        let options = Options {
            share_within_process: true,
            version: "1.2.3".to_owned(),
            endpoint: "http://127.0.0.1:7777".to_owned(),
        };
        let mut first = match acquire(&path, "manager", &options) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert_eq!(first.info().version, "1.2.3");
        assert_eq!(first.info().endpoint, "http://127.0.0.1:7777");

        let mut shared = match acquire(&path, "embedded", &options) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert!(!shared.owned(), "a shared handle does not own the lock");
        assert_eq!(shared.info().kind, "manager", "it reports the holder");
        // Releasing the shared handle is a no-op: the owner still holds it.
        shared.release()?;
        assert!(
            matches!(
                acquire(&path, "host", &Options::default()),
                Err(Error::Held(_))
            ),
            "the owner still holds the kernel lock"
        );
        first.release()?;
        let third = match acquire(&path, "host", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert!(third.owned());
        Ok(())
    }

    #[test]
    fn release_is_idempotent() -> io::Result<()> {
        let dir = scratch("idempotent")?;
        let path = dir.join("state.lock");
        let mut handle = match acquire(&path, "manager", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        handle.release()?;
        handle.release()?;
        assert!(!handle.owned());
        Ok(())
    }

    #[test]
    fn an_empty_path_is_refused() {
        assert!(matches!(
            acquire("", "manager", &Options::default()),
            Err(Error::EmptyPath)
        ));
        assert!(matches!(
            acquire("   ", "manager", &Options::default()),
            Err(Error::EmptyPath)
        ));
    }

    #[test]
    fn the_record_tolerates_a_file_that_is_not_one() -> io::Result<()> {
        let dir = scratch("garbage")?;
        let path = dir.join("garbage.lock");
        std::fs::write(&path, b"not json")?;
        assert_eq!(read_info(&path), None);
        // A bare pid line is what an older release left behind: no record, and
        // the lock stays acquirable.
        std::fs::write(&path, b"12345\n")?;
        assert_eq!(read_info(&path), None);
        let handle = match acquire(&path, "manager", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert_eq!(handle.info().pid, std::process::id());
        Ok(())
    }

    #[test]
    fn the_parent_directory_is_created() -> io::Result<()> {
        let dir = scratch("parent")?;
        let path = dir.join("nested/deeper/state.lock");
        let handle = match acquire(&path, "manager", &Options::default()) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        assert!(path.exists());
        assert_eq!(handle.path(), path);
        assert!(dir.join("nested/deeper").is_dir());
        Ok(())
    }

    #[test]
    fn timestamps_round_trip() {
        for (seconds, text) in [
            (0u64, "1970-01-01T00:00:00Z"),
            (86399, "1970-01-01T23:59:59Z"),
            (86_400, "1970-01-02T00:00:00Z"),
            (951_782_400, "2000-02-29T00:00:00Z"),
            (1_767_225_600, "2026-01-01T00:00:00Z"),
            (1_792_000_001, "2026-10-14T17:46:41Z"),
        ] {
            let at = UNIX_EPOCH + Duration::from_secs(seconds);
            assert_eq!(format_rfc3339_utc(at), text);
            assert_eq!(parse_rfc3339_utc(text), Some(at), "{text}");
        }
        assert_eq!(parse_rfc3339_utc("2026-10-14 17:46:41Z"), None);
        assert_eq!(parse_rfc3339_utc("2026-13-14T17:46:41Z"), None);
        assert_eq!(parse_rfc3339_utc(""), None);
        // Go's RFC3339 accepts these; this module writes neither, so neither
        // parses (see the module docs).
        assert_eq!(parse_rfc3339_utc("2026-10-14T17:46:41.500Z"), None);
        assert_eq!(parse_rfc3339_utc("2026-10-14T17:46:41+02:00"), None);
    }

    #[test]
    fn live_from_compares_the_record_with_the_clock() {
        let info = Info {
            pid: 1,
            started: "2026-10-14T17:46:41Z".to_owned(),
            ..Info::default()
        };
        let started = match parse_rfc3339_utc("2026-10-14T17:46:41Z") {
            Some(started) => started,
            None => panic!("the record's shape parses"),
        };
        assert!(info.live_from(started));
        assert!(info.live_from(started + Duration::from_secs(1)));
        assert!(!info.live_from(started - Duration::from_secs(1)));
        let empty = Info::default();
        assert!(!empty.live_from(started));
    }

    #[test]
    fn the_record_is_json_the_way_core_writes_it() -> io::Result<()> {
        let dir = scratch("record")?;
        let path = dir.join("state.lock");
        let at = UNIX_EPOCH + Duration::from_secs(1_792_000_001);
        let handle = match acquire_at(
            &path,
            "manager",
            &Options {
                version: "0.1.0".to_owned(),
                ..Options::default()
            },
            at,
        ) {
            Ok(handle) => handle,
            Err(error) => panic!("{error}"),
        };
        let raw = std::fs::read_to_string(&path)?;
        assert!(
            raw.ends_with('\n'),
            "the record is one line, newline-terminated"
        );
        let expected = format!(
            "{{\"pid\":{},\"kind\":\"manager\",\"started\":\"2026-10-14T17:46:41Z\",\"version\":\"0.1.0\"}}\n",
            std::process::id()
        );
        assert_eq!(raw, expected);
        assert_eq!(read_info(&path), Some(handle.info().clone()));
        Ok(())
    }
}
