//! What a host asks for when it installs the pipeline.
//!
//! `Install` is the only input to [`crate::install`]. Defaults are chosen per
//! [`Profile`] so a host that says nothing still gets sane output for its
//! shape: a long-running service, a one-shot command, or a client-side daemon.

use std::collections::HashMap;
use std::sync::Arc;

use crate::resource::Resource;
use crate::sinks::file;

/// Which kind of process is installing the pipeline.
///
/// The profile only sets *defaults*; every field of [`Install`] can override
/// them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Profile {
    /// A long-running server: systemd unit, container, Kubernetes pod.
    ///
    /// Structured output by default, timestamps on, `info` level.
    Service,
    /// A one-shot operator command (`check`, `catalog`, `ctl`).
    ///
    /// Human output, no target or timestamp, `warn` level: a command's result
    /// belongs on stdout and must not be buried in chatter.
    Cli,
    /// The platform running on a user's machine, usually as a daemon.
    ///
    /// Same shape as [`Profile::Service`], but text-first: nobody is scraping
    /// it, and a human may well be reading the terminal.
    Embedded,
}

impl Profile {
    /// The filter used when the host does not specify one.
    #[must_use]
    pub const fn default_directives(self) -> &'static str {
        match self {
            // `warn` keeps a command's stderr empty on success.
            Self::Cli => "warn",
            Self::Service | Self::Embedded => "info",
        }
    }
}

/// Where records and spans are shipped.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Otlp {
    /// Base endpoint, e.g. `http://otel-collector:4318`. The per-signal path is
    /// appended unless it is already present.
    pub endpoint: String,
    /// Headers sent on every export, for a managed backend's API key.
    pub headers: HashMap<String, String>,
    /// Skip TLS. Only for a collector on the same host or pod network.
    pub insecure: bool,
}

impl Otlp {
    /// The endpoint for one signal, honouring an endpoint that already names it.
    #[must_use]
    pub fn signal_endpoint(&self, path: &str) -> String {
        let base = self.endpoint.trim_end_matches('/');
        if base.ends_with(path) {
            base.to_owned()
        } else {
            format!("{base}{path}")
        }
    }
}

/// How a record becomes bytes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Format {
    /// `TS LEVEL target message key=value ...`
    Text,
    /// One JSON object per line.
    Json,
}

impl Format {
    /// Parse a host-supplied format name.
    #[must_use]
    pub fn parse(value: &str) -> Option<Self> {
        match value.trim().to_ascii_lowercase().as_str() {
            "text" | "plain" | "logfmt" => Some(Self::Text),
            "json" => Some(Self::Json),
            _ => None,
        }
    }
}

/// A fully resolved request to install the pipeline.
#[derive(Debug, Clone)]
pub struct Install {
    /// The host shape that supplied the defaults.
    pub profile: Profile,
    /// Rendering format.
    pub format: Format,
    /// Emit ANSI colours. Defaults to "stderr is a terminal".
    pub ansi: bool,
    /// Prefix text records with a timestamp.
    pub timestamps: bool,
    /// Prefix text records with the emitting target.
    pub targets: bool,
    /// Filter directives in `EnvFilter` syntax, e.g. `info,flowcraft_kernel=debug`.
    pub directives: String,
    /// Fields attached to every record, in [attribute order](crate::resource).
    pub resource: Arc<Resource>,
    /// Write to a rotating file as well as the streams.
    pub file: Option<file::Options>,
    /// Keep this many bytes of recent output in memory. Zero disables the ring.
    pub ring_bytes: usize,
    /// Replace the value of any field whose name looks like a credential.
    pub redact: bool,
    /// Ship spans and records to a collector. Only compiled with `otlp`.
    pub otlp: Option<Otlp>,
    /// Cap repeated warnings and errors. `None` disables it.
    pub throttle: Option<crate::throttle::Policy>,
    /// Write the memory ring here the first time an `ERROR` is recorded.
    ///
    /// A client daemon's ring dies with the process, which is exactly when
    /// someone wants to read it. Defaults to the log file's `.last-error`
    /// sibling; `None` keeps the ring in memory only.
    pub spill: Option<std::path::PathBuf>,
}

impl Install {
    /// Defaults for `profile` with an empty resource.
    #[must_use]
    pub fn new(profile: Profile) -> Self {
        Self {
            profile,
            format: Format::Text,
            ansi: false,
            timestamps: profile != Profile::Cli,
            targets: profile != Profile::Cli,
            directives: profile.default_directives().to_owned(),
            resource: Arc::new(Resource::default()),
            file: None,
            ring_bytes: 0,
            redact: true,
            otlp: None,
            throttle: Some(crate::throttle::Policy::default()),
            spill: None,
        }
    }

    /// Override the filter directives.
    #[must_use]
    pub fn with_directives(mut self, directives: impl Into<String>) -> Self {
        self.directives = directives.into();
        self
    }

    /// Override the rendering format.
    #[must_use]
    pub fn with_format(mut self, format: Format) -> Self {
        self.format = format;
        self
    }

    /// Override ANSI colouring.
    #[must_use]
    pub fn with_ansi(mut self, ansi: bool) -> Self {
        self.ansi = ansi;
        self
    }

    /// Attach the fields every record carries.
    #[must_use]
    pub fn with_resource(mut self, resource: Resource) -> Self {
        self.resource = Arc::new(resource);
        self
    }

    /// Also write to a rotating file.
    #[must_use]
    pub fn with_file(mut self, options: file::Options) -> Self {
        self.file = Some(options);
        self
    }

    /// Keep the most recent `bytes` of output in memory.
    #[must_use]
    pub fn with_ring(mut self, bytes: usize) -> Self {
        self.ring_bytes = bytes;
        self
    }

    /// Turn the credential-name redaction on or off (it is on by default).
    #[must_use]
    pub fn with_redaction(mut self, redact: bool) -> Self {
        self.redact = redact;
        self
    }

    /// Ship spans and records to a collector.
    #[must_use]
    pub fn with_otlp(mut self, otlp: Otlp) -> Self {
        self.otlp = Some(otlp);
        self
    }

    /// Cap repeated warnings and errors.
    #[must_use]
    pub fn with_throttle(mut self, policy: crate::throttle::Policy) -> Self {
        self.throttle = Some(policy);
        self
    }

    /// Write the ring to `path` when an error is recorded.
    #[must_use]
    pub fn with_spill(mut self, path: impl Into<std::path::PathBuf>) -> Self {
        self.spill = Some(path.into());
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cli_defaults_keep_stderr_quiet() {
        let install = Install::new(Profile::Cli);
        assert_eq!(install.directives, "warn");
        assert!(!install.timestamps);
        assert!(!install.targets);
    }

    #[test]
    fn formats_parse_case_insensitively() {
        assert_eq!(Format::parse("JSON"), Some(Format::Json));
        assert_eq!(Format::parse(" text "), Some(Format::Text));
        assert_eq!(Format::parse("xml"), None);
    }
}
