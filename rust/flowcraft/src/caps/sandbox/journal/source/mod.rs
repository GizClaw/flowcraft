//! The syscall layer, behind a seam.
//!
//! A source does one thing: turn whatever the platform reports into [`RawEvent`]s
//! on a channel, plus a gap whenever it knows coverage broke. Everything a
//! consumer depends on — folding, retention, cursor accounting — lives above it,
//! so a platform can be swapped without changing the contract.

mod notify;

use super::{GapReason, RawEvent};
pub(crate) use notify::NotifySource;

/// Something a source reports.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Incoming {
    /// One notification.
    Event(RawEvent),
    /// Coverage is not guaranteed from here on.
    Gap(GapReason),
}

/// Where a source sends what it sees.
///
/// Unbounded on purpose: the callback runs on the watcher's own thread and must
/// never block, and the consumer is a task that drains immediately.
pub(crate) type Sink = tokio::sync::mpsc::UnboundedSender<Incoming>;
