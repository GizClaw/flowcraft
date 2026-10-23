//! Installing the pipeline.
//!
//! A process gets exactly one subscriber, so there is exactly one entry point
//! and one owner: [`install`] returns a [`Guard`] the host keeps until exit.
//! The filter alone can be replaced later through the guard, which is what a
//! host needs when the log level comes from a configuration file it has not
//! parsed yet.
//!
//! # Why not `try_init` in a library
//!
//! Capability crates depend on this crate only for the field vocabulary; they
//! must never install anything, or a host embedding two of them would get
//! whichever one started first. Nothing here runs unless a host calls it.

use std::fmt;
use std::path::Path;
use std::sync::Arc;

use flowcraft_error::{Error, Result};
use tracing_subscriber::filter::EnvFilter;
use tracing_subscriber::layer::Layered;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::registry::Registry;
use tracing_subscriber::reload;
use tracing_subscriber::util::SubscriberInitExt;

use crate::format::json::Json;
use crate::format::text::Text;
use crate::options::{Format, Install, Profile};
#[cfg(feature = "otel")]
use crate::otel;
use crate::sinks::{self, Sinks, file, ring};
use std::path::PathBuf;

/// Owns the installed pipeline.
///
/// Keep it alive for the life of the process; dropping it only flushes. A host
/// that could not install anything (a second call, an unknown directive) can
/// still hold a [`Guard`] so its shutdown path needs no special case.
pub struct Guard {
    handle: Option<reload::Handle<EnvFilter, Registry>>,
    ring: Option<Arc<ring::Shared>>,
    file: Option<Arc<file::FileSink>>,
    #[cfg(feature = "otel")]
    tracer: Option<opentelemetry_sdk::trace::TracerProvider>,
    logger: crate::bridge::Logger,
}

impl fmt::Debug for Guard {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Guard")
            .field("installed", &self.handle.is_some())
            .field("ring", &self.ring.as_ref().map(|ring| ring.capacity()))
            .field("file", &self.file.as_ref().map(|sink| sink.path()))
            .finish()
    }
}

impl Guard {
    /// A guard for a process that never installed a pipeline.
    #[must_use]
    pub fn disabled() -> Self {
        Self {
            handle: None,
            ring: None,
            file: None,
            #[cfg(feature = "otel")]
            tracer: None,
            logger: crate::bridge::Logger::default(),
        }
    }

    /// Whether a pipeline is installed behind this guard.
    #[must_use]
    pub fn is_installed(&self) -> bool {
        self.handle.is_some()
    }

    /// Replace the filter while the process runs.
    ///
    /// This is the hook a host uses after reading its configuration: the level
    /// is decided there, but the pipeline had to exist before the file could be
    /// read, so the level cannot have been known at install time.
    pub fn set_directives(&self, directives: &str) -> Result<()> {
        let Some(handle) = self.handle.as_ref() else {
            return Ok(());
        };
        let filter = build_filter(directives)?;
        handle
            .modify(|current| *current = filter)
            .map_err(|_| Error::internal("telemetry: the subscriber was dropped"))
    }

    /// The directives currently in force.
    pub fn directives(&self) -> Option<String> {
        let handle = self.handle.as_ref()?;
        handle.with_current(|filter| filter.to_string()).ok()
    }

    /// The recent output held in memory, when a ring is installed.
    ///
    /// This is what a support bundle attaches: the records from just before
    /// whatever went wrong, which a rotating file has often already lost.
    #[must_use]
    pub fn ring_dump(&self) -> Option<String> {
        self.ring.as_ref().map(|ring| ring.dump())
    }

    /// How many bytes the ring holds, when one is installed.
    #[must_use]
    pub fn ring_len(&self) -> Option<usize> {
        self.ring.as_ref().map(|ring| ring.len())
    }

    /// The active log file, when a file sink is installed.
    #[must_use]
    pub fn file_path(&self) -> Option<&Path> {
        self.file.as_ref().map(|sink| sink.path())
    }

    /// Whether the file sink stopped writing after a failure.
    ///
    /// A daemon that lost its log file is still healthy, but an operator needs
    /// to be told, and a support bundle must not pretend the file is complete.
    #[must_use]
    pub fn file_disabled(&self) -> Option<bool> {
        self.file.as_ref().map(|sink| sink.is_disabled())
    }

    /// The tracer provider, when trace context is installed.
    #[cfg(feature = "otel")]
    #[must_use]
    pub fn tracer_provider(&self) -> Option<&opentelemetry_sdk::trace::TracerProvider> {
        self.tracer.as_ref()
    }

    /// Push buffered output to every destination.
    pub fn flush(&self) {
        if let Some(file) = &self.file {
            file.flush();
        }
        sinks::stderr::flush();
    }

    /// Flush before the process exits.
    ///
    /// Takes `&self` so the guard can be shared with an admin surface; the
    /// `Drop` impl flushes anyway, this just makes the intent explicit at the
    /// shutdown call site.
    pub fn shutdown(&self) {
        self.flush();
        #[cfg(feature = "otel")]
        if let Some(provider) = &self.tracer {
            // Drain the exporter, if there is one; a no-op otherwise.
            let _ = provider.shutdown();
        }
        self.logger.shutdown();
    }
}

impl Drop for Guard {
    fn drop(&mut self) {
        sinks::stderr::flush();
    }
}

/// Install the pipeline for this process.
///
/// Fails only when the filter directives are invalid or a subscriber is already
/// installed; a host should treat both as "run without telemetry", not as a
/// reason to exit.
pub fn install(install: Install) -> Result<Guard> {
    let filter = build_filter(&install.directives)?;
    let (filter_layer, handle) = reload::Layer::new(filter);
    let resource = install.resource.clone();
    let redact = install.redact;

    // A command's stdout is its result, never its log.
    let ring = (install.ring_bytes > 0).then(|| Arc::new(ring::Shared::new(install.ring_bytes)));
    let file = match &install.file {
        Some(options) => Some(Arc::new(file::FileSink::open(options.clone()).map_err(
            |error| {
                Error::not_available(format!(
                    "telemetry: cannot open the log file {}: {error}",
                    options.path.display()
                ))
            },
        )?)),
        None => None,
    };
    // The spill defaults to the log file's sibling: a client that configured a
    // log wants the failing window on disk, and one that did not asked for
    // nothing to be written at all.
    let spill = install
        .spill
        .clone()
        .or_else(|| file.as_ref().map(|sink| spill_path(sink.path())))
        .filter(|_| install.ring_bytes > 0)
        .map(sinks::Spill::new);
    let throttle = install
        .throttle
        .filter(crate::throttle::Policy::is_enabled)
        .map(|policy| Arc::new(crate::throttle::Throttle::new(policy)));
    let writer = Sinks {
        all_to_stderr: install.profile == Profile::Cli,
        file: file.clone(),
        ring: ring.clone(),
        spill,
    };
    // Layer order matters: the log bridge needs the span context, the span
    // layer needs the filter, and the renderer reads the span it is inside.
    type Filtered = Layered<reload::Layer<EnvFilter, Registry>, Registry>;
    #[cfg(feature = "otel")]
    type Bridged = Layered<crate::bridge::Bridge<Filtered>, Filtered>;

    let (bridge, logger) = crate::bridge::layer::<Filtered>(&install);
    let filtered = registry().with(filter_layer).with(bridge);
    #[cfg(feature = "otel")]
    let (base, tracer) = {
        let provider = otel::provider(&install.resource, install.otlp.as_ref());
        (
            filtered.with(otel::layer::<Bridged>(&provider)),
            Some(provider),
        )
    };
    #[cfg(not(feature = "otel"))]
    let base = filtered;

    let initialised = match install.format {
        Format::Text => {
            let formatter = Text {
                timestamps: install.timestamps,
                targets: install.targets,
                ansi: install.ansi,
                resource,
                redact,
                throttle: throttle.clone(),
            };
            base.with(
                tracing_subscriber::fmt::layer()
                    .with_ansi(false)
                    .with_writer(writer)
                    .event_format(formatter),
            )
            .try_init()
        }
        Format::Json => {
            let formatter = Json {
                resource,
                redact,
                throttle: throttle.clone(),
            };
            base.with(
                tracing_subscriber::fmt::layer()
                    .with_ansi(false)
                    .with_writer(writer)
                    .event_format(formatter),
            )
            .try_init()
        }
    };
    initialised.map_err(|error| {
        Error::not_available(format!("telemetry: cannot install the pipeline: {error}"))
    })?;

    Ok(Guard {
        handle: Some(handle),
        ring,
        file,
        #[cfg(feature = "otel")]
        tracer,
        logger,
    })
}

fn registry() -> Registry {
    tracing_subscriber::registry()
}

/// Where the error-triggered ring dump lands: next to the log file.
fn spill_path(log: &Path) -> PathBuf {
    let mut name = log.as_os_str().to_owned();
    name.push(".last-error");
    PathBuf::from(name)
}

fn build_filter(directives: &str) -> Result<EnvFilter> {
    EnvFilter::try_new(directives).map_err(|error| {
        Error::validation(format!(
            "telemetry: invalid filter directives {directives:?}: {error}"
        ))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn invalid_directives_are_a_validation_error() {
        let error = build_filter("info,=trace").expect_err("should reject");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }

    #[test]
    fn a_disabled_guard_is_a_noop() {
        let guard = Guard::disabled();
        assert!(!guard.is_installed());
        assert!(guard.set_directives("debug").is_ok());
        guard.flush();
        guard.shutdown();
    }
}
