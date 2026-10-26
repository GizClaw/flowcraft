//! The platform's telemetry pipeline: one place that decides what is recorded,
//! how it is rendered, and where it goes.
//!
//! # Scope
//!
//! * **Vocabulary** — [`attrs`] names every well-known field, and [`logerr!`](crate::logerr)
//!   emits a failure together with its [`crate::error::ErrorKind`].
//! * **Host** — [`install`] builds the subscriber once per process and returns
//!   a [`Guard`] whose filter can be replaced when the host finally knows its
//!   configuration.
//! * **Rendering** — [`options::Format::Text`] for humans, [`options::Format::Json`]
//!   for machines. One record is always exactly one line.
//! * **Routing** — below `WARN` to stdout, `WARN` and above to stderr.
//!
//! # Out of scope
//!
//! Machine identity (that is `crate::kernel::identity`), capability payloads,
//! and anything a library crate should decide for itself. Capability crates
//! depend on this crate with default features off and never install anything.
//!
//! # Authoring rules
//!
//! * `error` is never rendered with `%error` alone: use [`logerr!`](crate::logerr), which adds
//!   `error.kind` and `error.message` as separate fields.
//! * Field names come from [`attrs`]; a call site spells the dotted name out
//!   because `tracing` accepts only literals there.
//! * One record is one fact. A loop that emits per iteration is `debug`.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod attrs;
pub mod test_support;

mod bridge;
#[cfg(feature = "otlp")]
mod export;
mod format;
mod install;
mod options;
#[cfg(feature = "otel")]
mod otel;
mod resource;
pub mod sinks;
pub mod throttle;

/// Re-exported so call sites need one dependency, not two.
///
/// ```ignore
/// use flowcraft::telemetry::{info, attrs::attr};
/// info!(flowcraft.craft = "alpha", "craft started");
/// ```
pub use tracing;
pub use tracing::{debug, error, info, instrument, trace, warn};

#[cfg(feature = "otel")]
pub use otel::propagation;
#[cfg(feature = "otel")]
pub use otel::{ids as trace_ids, set_parent};

pub use attrs::attr;
pub use install::{Guard, install};
pub use options::{Format, Install, Otlp, Profile};
pub use resource::{Resource, session_id};

/// Emit a failure with its classification attached.
///
/// `%error` alone renders only the message, because [`crate::error::Error`]
/// implements `Display` as the message alone — the category, which is the part
/// dashboards filter on, would be lost. This macro records both.
///
/// The message comes first, then the optional fields:
///
/// ```ignore
/// logerr!(warn, error, "craft stop failed", flowcraft.craft = %name);
/// // -> WARN craft stop failed flowcraft.craft=alpha error.kind=not_available
/// ```
///
/// Unlike `tracing` (which wants fields before the message), the message is the
/// third argument because the macro appends its own fields to the end.
///
/// `#[macro_export]` puts it at the crate root (`flowcraft::logerr!`), which is
/// where a macro has to live: a macro that must be callable from other modules
/// cannot be a module item.
#[macro_export]
macro_rules! logerr {
    ($level:ident, $error:expr, $message:expr) => {{
        let __failure = &$error;
        $crate::telemetry::tracing::$level!(
            error.kind = $crate::telemetry::tracing::field::display(__failure.kind().as_str()),
            error.message = $crate::telemetry::tracing::field::display(__failure.message()),
            $message
        )
    }};
    ($level:ident, $error:expr, $message:expr, $($fields:tt)+) => {{
        let __failure = &$error;
        $crate::telemetry::tracing::$level!(
            $($fields)+ ,
            error.kind = $crate::telemetry::tracing::field::display(__failure.kind().as_str()),
            error.message = $crate::telemetry::tracing::field::display(__failure.message()),
            $message
        )
    }};
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::Error;

    #[test]
    fn logerr_attaches_the_error_kind() {
        let capture = test_support::Capture::new();
        capture.with(|| {
            let failure = Error::not_available("no collector");
            logerr!(warn, failure, "export failed", flowcraft.craft = "alpha");
        });
        let record = capture.find("export failed").expect("record");
        assert_eq!(record.level, tracing::Level::WARN);
        assert_eq!(record.field(attrs::attr::ERROR_KIND), Some("not_available"));
        assert_eq!(
            record.field(attrs::attr::ERROR_MESSAGE),
            Some("no collector")
        );
        assert_eq!(record.field(attrs::attr::CRAFT), Some("alpha"));
    }

    #[test]
    fn logerr_works_without_extra_fields() {
        let capture = test_support::Capture::new();
        capture.with(|| {
            let failure = Error::timeout("drain");
            logerr!(error, failure, "shutdown failed");
        });
        let record = capture.find("shutdown failed").expect("record");
        assert_eq!(record.level, tracing::Level::ERROR);
        assert_eq!(record.field(attrs::attr::ERROR_KIND), Some("timeout"));
    }
}
