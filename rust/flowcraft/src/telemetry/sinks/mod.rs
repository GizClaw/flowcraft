//! Where rendered lines go.
//!
//! One writer fans a finished record out to every enabled destination. Doing the
//! tee here rather than as separate `Layer`s means a record is formatted exactly
//! once, and the memory ring holds byte-for-byte what the file and the terminal
//! got — a support bundle that disagrees with the log is worse than no bundle.

pub mod file;
pub mod ring;
pub(crate) mod stderr;

use std::io::{self, Write};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tracing::Level;
use tracing_subscriber::fmt::MakeWriter;

use stderr::Stream;

/// The destinations enabled for this process.
#[derive(Debug)]
pub(crate) struct Sinks {
    /// Send records below `WARN` to stderr too (a command prints its result on
    /// stdout, so its log must not share the stream).
    pub(crate) all_to_stderr: bool,
    pub(crate) file: Option<Arc<file::FileSink>>,
    pub(crate) ring: Option<Arc<ring::Shared>>,
    /// Where the ring is written when something goes wrong, and when it last
    /// was.
    pub(crate) spill: Option<Spill>,
}

/// The error-triggered ring dump.
#[derive(Debug)]
pub(crate) struct Spill {
    path: PathBuf,
    last: Mutex<Option<Instant>>,
}

/// How often a spill may happen, so a failing loop cannot spin the disk.
const SPILL_EVERY: Duration = Duration::from_secs(60);

impl Spill {
    pub(crate) fn new(path: PathBuf) -> Self {
        Self {
            path,
            last: Mutex::new(None),
        }
    }

    /// Write the ring next to the log, at most once a minute.
    fn maybe_write(&self, ring: &ring::Shared) {
        let Ok(mut last) = self.last.lock() else {
            return;
        };
        if let Some(at) = *last
            && at.elapsed() < SPILL_EVERY
        {
            return;
        }
        *last = Some(Instant::now());
        if let Some(parent) = self.path.parent()
            && !parent.as_os_str().is_empty()
        {
            let _ = std::fs::create_dir_all(parent);
        }
        // Best effort, like every other sink: the failure path must not fail.
        let _ = std::fs::write(&self.path, ring.dump());
    }
}

impl<'a> MakeWriter<'a> for Sinks {
    type Writer = SinkWriter<'a>;

    fn make_writer(&'a self) -> Self::Writer {
        SinkWriter {
            sinks: self,
            stream: if self.all_to_stderr {
                Stream::Err
            } else {
                Stream::Out
            },
            level: Level::INFO,
        }
    }

    fn make_writer_for(&'a self, metadata: &tracing::Metadata<'_>) -> Self::Writer {
        let level = *metadata.level();
        let stream = if self.all_to_stderr {
            Stream::Err
        } else {
            Stream::for_level(level)
        };
        SinkWriter {
            sinks: self,
            stream,
            level,
        }
    }
}

/// Writes one record to every enabled destination.
pub(crate) struct SinkWriter<'a> {
    sinks: &'a Sinks,
    stream: Stream,
    level: Level,
}

impl Write for SinkWriter<'_> {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        // The ring first: it is the copy that survives a crash.
        if let Some(ring) = &self.sinks.ring {
            ring.push(buf);
        }
        if let Some(file) = &self.sinks.file {
            file.write(buf);
        }
        let written = self.stream.write(buf);
        // An error is the one moment the ring is worth keeping: it dies with the
        // process, and it holds the records that explain the error.
        if self.level == Level::ERROR
            && let (Some(spill), Some(ring)) = (&self.sinks.spill, &self.sinks.ring)
        {
            spill.maybe_write(ring);
        }
        written
    }

    fn flush(&mut self) -> io::Result<()> {
        if let Some(file) = &self.sinks.file {
            file.flush();
        }
        self.stream.flush()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sinks(dir: &tempfile::TempDir) -> (Sinks, Arc<ring::Shared>) {
        let ring = Arc::new(ring::Shared::new(4096));
        let file = file::FileSink::open(file::Options::new(dir.path().join("log"))).expect("open");
        (
            Sinks {
                all_to_stderr: false,
                file: Some(Arc::new(file)),
                ring: Some(ring.clone()),
                spill: None,
            },
            ring,
        )
    }

    #[test]
    fn one_record_reaches_the_ring_and_the_file_identically() {
        let dir = tempfile::tempdir().expect("tempdir");
        let (sinks, ring) = sinks(&dir);
        let mut writer = sinks.make_writer();
        writer.write_all(b"line one\n").expect("write");
        writer.flush().expect("flush");
        assert_eq!(ring.dump(), "line one\n");
        assert_eq!(
            std::fs::read_to_string(dir.path().join("log")).unwrap_or_default(),
            "line one\n"
        );
    }

    #[test]
    fn a_command_sends_low_severities_to_stderr() {
        let dir = tempfile::tempdir().expect("tempdir");
        let (mut sinks, _ring) = sinks(&dir);
        sinks.all_to_stderr = true;
        assert!(matches!(sinks.make_writer().stream, Stream::Err));
    }
}
