//! A rotating local log file.
//!
//! A daemon on a user's machine has no logrotate and no collector, so the file
//! sink owns its own bound: once `max_bytes` is reached the file is rotated and
//! the oldest copy dropped. Writing must never be able to fail the process, so a
//! write error disables the sink after saying so once on stderr — losing logs is
//! annoying, hanging because a disk filled up is an outage.
//!
//! The rotation is size-triggered only. Time-based rotation, compression and
//! shipping belong to whatever collects the directory, not to the process that
//! is trying to stay small.

use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// Where and how large.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Options {
    /// The active file. Rotated copies live beside it as `<name>.1`, `.2`, ...
    pub path: PathBuf,
    /// Size that triggers a rotation. Zero disables rotation.
    pub max_bytes: u64,
    /// How many rotated copies to keep.
    pub backups: u8,
}

impl Options {
    /// Options for `path` with the daemon defaults: 8 MiB, three copies.
    #[must_use]
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self {
            path: path.into(),
            max_bytes: 8 * 1024 * 1024,
            backups: 3,
        }
    }
}

/// A file that rotates under a size bound.
#[derive(Debug)]
pub(crate) struct FileSink {
    state: Mutex<State>,
    path: PathBuf,
    max_bytes: u64,
    backups: u8,
}

#[derive(Debug)]
struct State {
    writer: Option<File>,
    written: u64,
    disabled: bool,
}

impl FileSink {
    /// Open (or create) the file, rotating first if it is already too large.
    pub(crate) fn open(options: Options) -> std::io::Result<Self> {
        let sink = Self {
            state: Mutex::new(State {
                writer: None,
                written: 0,
                disabled: false,
            }),
            path: options.path,
            max_bytes: options.max_bytes,
            backups: options.backups,
        };
        sink.reopen()?;
        Ok(sink)
    }

    pub(crate) fn path(&self) -> &Path {
        &self.path
    }

    /// Append `bytes`, rotating when the file would exceed its bound.
    ///
    /// Never returns an error: the caller is a formatting layer that has nowhere
    /// to put one, and a logging failure is not a reason to stop working.
    pub(crate) fn write(&self, bytes: &[u8]) {
        let Ok(mut state) = self.state.lock() else {
            return;
        };
        if state.disabled {
            return;
        }
        if self.max_bytes > 0
            && state.written > 0
            && state.written + bytes.len() as u64 > self.max_bytes
            && let Err(error) = self.rotate(&mut state)
        {
            self.disable(&mut state, &error);
            return;
        }
        let Some(writer) = state.writer.as_mut() else {
            return;
        };
        // Written straight through, not buffered: an operator who opens the
        // file expects to see what just happened, and a crash must not cost the
        // last few kilobytes.
        match writer.write_all(bytes) {
            Ok(()) => state.written += bytes.len() as u64,
            Err(error) => self.disable(&mut state, &error),
        }
    }

    pub(crate) fn flush(&self) {
        if let Ok(mut state) = self.state.lock()
            && let Some(writer) = state.writer.as_mut()
        {
            let _ = writer.flush();
        }
    }

    pub(crate) fn is_disabled(&self) -> bool {
        self.state
            .lock()
            .map(|state| state.disabled)
            .unwrap_or(true)
    }

    fn reopen(&self) -> std::io::Result<()> {
        if let Some(parent) = self.path.parent()
            && !parent.as_os_str().is_empty()
        {
            std::fs::create_dir_all(parent)?;
        }
        let mut state = self
            .state
            .lock()
            .map_err(|_| std::io::Error::other("log file lock poisoned"))?;
        if self.max_bytes > 0 && state.written >= self.max_bytes {
            self.rotate(&mut state)?;
        }
        let file = open_private(&self.path)?;
        state.written = file.metadata().map(|meta| meta.len()).unwrap_or(0);
        state.writer = Some(file);
        Ok(())
    }

    /// Shift `<path>` → `<path>.1` → ... → `<path>.backups`, dropping the last.
    fn rotate(&self, state: &mut State) -> std::io::Result<()> {
        state.writer = None;
        if self.backups == 0 {
            // No history wanted: truncate rather than grow forever.
            let file = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .open(&self.path)?;
            state.written = 0;
            state.writer = Some(file);
            return Ok(());
        }
        remove_if_present(&rotated(&self.path, u32::from(self.backups)))?;
        for index in (1..self.backups).rev() {
            let from = rotated(&self.path, u32::from(index));
            let to = rotated(&self.path, u32::from(index) + 1);
            rename_if_present(&from, &to)?;
        }
        rename_if_present(&self.path, &rotated(&self.path, 1))?;

        let file = open_private(&self.path)?;
        state.written = 0;
        state.writer = Some(file);
        Ok(())
    }

    /// Stop writing and say so once, on stderr.
    ///
    /// This is the one write in the crate that bypasses the pipeline: the
    /// pipeline is what failed.
    fn disable(&self, state: &mut State, error: &std::io::Error) {
        state.disabled = true;
        state.writer = None;
        eprintln!(
            "flowcraft-telemetry: log file {} disabled after a write error: {error}",
            self.path.display()
        );
    }
}

fn rotated(path: &Path, index: u32) -> PathBuf {
    let mut name = path.as_os_str().to_owned();
    name.push(format!(".{index}"));
    PathBuf::from(name)
}

fn open_private(path: &Path) -> std::io::Result<File> {
    let file = OpenOptions::new().create(true).append(true).open(path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        // A log file can carry paths and tenant names; it is not world-readable.
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    Ok(file)
}

fn remove_if_present(path: &Path) -> std::io::Result<()> {
    match std::fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

fn rename_if_present(from: &Path, to: &Path) -> std::io::Result<()> {
    match std::fs::rename(from, to) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sink(dir: &tempfile::TempDir, max_bytes: u64, backups: u8) -> FileSink {
        FileSink::open(Options {
            path: dir.path().join("flowcraftd.log"),
            max_bytes,
            backups,
        })
        .expect("open")
    }

    fn read(path: &Path) -> String {
        std::fs::read_to_string(path).unwrap_or_default()
    }

    #[test]
    fn appends_across_sessions() {
        let dir = tempfile::tempdir().expect("tempdir");
        {
            let sink = sink(&dir, 1_000, 3);
            sink.write(b"one\n");
            sink.flush();
        }
        {
            let sink = sink(&dir, 1_000, 3);
            sink.write(b"two\n");
            sink.flush();
        }
        assert_eq!(read(&dir.path().join("flowcraftd.log")), "one\ntwo\n");
    }

    #[test]
    fn rotates_when_full_and_keeps_the_configured_history() {
        let dir = tempfile::tempdir().expect("tempdir");
        let sink = sink(&dir, 8, 2);
        for line in [b"aaaaaaaa\n", b"bbbbbbbb\n", b"cccccccc\n"] {
            sink.write(line);
        }
        sink.flush();
        let active = dir.path().join("flowcraftd.log");
        let first = dir.path().join("flowcraftd.log.1");
        let second = dir.path().join("flowcraftd.log.2");
        let third = dir.path().join("flowcraftd.log.3");
        assert!(active.exists() && first.exists() && second.exists());
        assert!(!third.exists(), "only two backups are kept");
        assert_eq!(read(&active), "cccccccc\n");
        assert_eq!(read(&first), "bbbbbbbb\n");
        assert_eq!(read(&second), "aaaaaaaa\n");
    }

    #[test]
    fn a_file_larger_than_the_bound_rotates_at_open() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::write(dir.path().join("flowcraftd.log"), "x".repeat(64)).expect("write");
        let sink = sink(&dir, 32, 2);
        sink.write(b"fresh\n");
        sink.flush();
        assert_eq!(read(&dir.path().join("flowcraftd.log")), "fresh\n");
        assert!(dir.path().join("flowcraftd.log.1").exists());
    }

    #[test]
    fn zero_backups_truncates_instead_of_rotating() {
        let dir = tempfile::tempdir().expect("tempdir");
        let sink = sink(&dir, 4, 0);
        sink.write(b"aaaa\n");
        sink.write(b"bbbb\n");
        sink.flush();
        assert_eq!(read(&dir.path().join("flowcraftd.log")), "bbbb\n");
        assert!(!dir.path().join("flowcraftd.log.1").exists());
    }

    #[cfg(unix)]
    #[test]
    fn a_rotation_that_cannot_complete_disables_the_sink() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().expect("tempdir");
        let sink = sink(&dir, 8, 2);
        sink.write(b"aaaaa\n");
        sink.flush();

        // A read-only directory makes the rename fail, which is the failure a
        // full disk or a lost mount looks like from here.
        std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o555))
            .expect("chmod");
        sink.write(b"bbbbb\n");
        let disabled = sink.is_disabled();
        // Restore before asserting, so a failure still cleans up.
        let _ = std::fs::set_permissions(dir.path(), std::fs::Permissions::from_mode(0o755));
        assert!(disabled, "a failed rotation must disable the sink");
        sink.write(b"after\n");
        assert!(sink.is_disabled(), "a disabled sink stays disabled");
    }

    #[cfg(unix)]
    #[test]
    fn the_file_is_owner_only() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().expect("tempdir");
        let sink = sink(&dir, 1_000, 1);
        sink.write(b"line\n");
        sink.flush();
        let mode = std::fs::metadata(dir.path().join("flowcraftd.log"))
            .expect("metadata")
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o600);
    }
}
