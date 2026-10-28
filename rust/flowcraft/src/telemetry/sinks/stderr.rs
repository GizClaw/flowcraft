//! The two standard streams.
//!
//! Which one a record belongs on is decided by [`Sinks`](super::Sinks), because
//! that decision depends on the profile; this module only knows how to write to
//! each.

use std::io::{self, Write};

use tracing::Level;

/// One of the two standard streams.
pub(crate) enum Stream {
    /// Diagnostics that are part of normal operation.
    Out,
    /// Anything an operator may need to act on.
    Err,
}

impl Stream {
    /// The POSIX split. `tracing::Level` orders by *verbosity*, not severity (a
    /// trace is "greater" than an error), so the mapping is spelled out.
    pub(crate) fn for_level(level: Level) -> Self {
        if matches!(level, Level::WARN | Level::ERROR) {
            Self::Err
        } else {
            Self::Out
        }
    }
}

impl Write for Stream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        match self {
            Self::Out => io::stdout().write(buf),
            Self::Err => io::stderr().write(buf),
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        match self {
            Self::Out => io::stdout().flush(),
            Self::Err => io::stderr().flush(),
        }
    }
}

/// Flush both streams.
pub(crate) fn flush() {
    let _ = io::stdout().flush();
    let _ = io::stderr().flush();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn warnings_and_errors_go_to_stderr() {
        assert!(matches!(Stream::for_level(Level::WARN), Stream::Err));
        assert!(matches!(Stream::for_level(Level::ERROR), Stream::Err));
    }

    #[test]
    fn routine_records_go_to_stdout() {
        assert!(matches!(Stream::for_level(Level::INFO), Stream::Out));
        assert!(matches!(Stream::for_level(Level::DEBUG), Stream::Out));
        assert!(matches!(Stream::for_level(Level::TRACE), Stream::Out));
    }
}
