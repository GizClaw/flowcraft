//! The retention ring: a bounded, cursor-based event window.
//!
//! The invariant that makes the stream usable is that `(after_seq, next_seq]` is
//! always accounted for: every sequence number in it was either returned or
//! declared lost. A [`Gap`] is a statement about **coverage**, not an error and
//! not an empty range — a consumer must render it as "this may be incomplete",
//! never as "nothing else happened".
//!
//! Unlike a session's output log, [`Ring::read`] never blocks: a journal consumer
//! drains on its own schedule, the window is bounded, and a reader that falls
//! behind is *told* it fell behind.

use std::collections::VecDeque;

use super::{Batch, FileOp, Gap, GapReason, WriteEvent};

/// How many hole markers to remember. A hole older than the retained window is
/// subsumed by the retention gap.
const MAX_GAPS: usize = 16;

/// The bounded window of one journal.
#[derive(Debug)]
pub(crate) struct Ring {
    events: VecDeque<WriteEvent>,
    /// Holes, oldest first: coverage is not guaranteed from `seq` onwards.
    gaps: VecDeque<(u64, GapReason)>,
    /// Sequence number the next event or hole will get.
    next_seq: u64,
    /// How many events to keep.
    retention: usize,
    /// Set once the window dropped something.
    dropped: bool,
    closed: bool,
}

impl Ring {
    pub(crate) fn new(retention: usize) -> Self {
        Self {
            events: VecDeque::new(),
            gaps: VecDeque::new(),
            next_seq: 1,
            // `0` would mean "keep nothing", which is never what a caller wants.
            retention: retention.max(1),
            dropped: false,
            closed: false,
        }
    }

    /// Append one observed change.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn push(
        &mut self,
        op: FileOp,
        path: String,
        old_path: Option<String>,
        is_dir: bool,
        at_unix_nano: u64,
        dev: Option<u64>,
        ino: Option<u64>,
        size: Option<i64>,
    ) {
        if self.closed {
            return;
        }
        let seq = self.next_seq;
        self.next_seq += 1;
        self.events.push_back(WriteEvent {
            seq,
            at_unix_nano,
            op,
            path,
            old_path,
            is_dir,
            dev,
            ino,
            size,
            // Attribution is not guessable: inotify cannot name the writer
            // without privileges and kqueue cannot name it at all.
            session: String::new(),
        });
        while self.events.len() > self.retention {
            self.events.pop_front();
            self.dropped = true;
        }
        if let Some(front) = self.events.front().map(|event| event.seq) {
            self.gaps.retain(|(seq, _)| *seq > front);
        }
    }

    /// Record that coverage is not guaranteed from here on.
    pub(crate) fn gap(&mut self, reason: GapReason) {
        if self.closed {
            return;
        }
        let seq = self.next_seq;
        self.next_seq += 1;
        self.gaps.push_back((seq, reason));
        while self.gaps.len() > MAX_GAPS {
            self.gaps.pop_front();
        }
        // A hole makes the window useless for replay: a consumer that reads it
        // would silently get "before the hole" data with no marker. Drop it and
        // let the retention gap take over.
        self.events.clear();
        self.dropped = true;
    }

    /// Freeze the journal: no further events arrive, the residue stays readable.
    pub(crate) fn close(&mut self) {
        self.closed = true;
    }

    /// Read from `after`, at most `max` events. Never blocks.
    ///
    /// The cursor convention: `after` is the **last sequence number the caller
    /// has consumed** (`0` means "nothing yet"), so events with `seq > after` are
    /// returned and `Batch::next_seq` is the cursor to pass next time. A batch
    /// either delivers events or declares a hole — never both, so the caller's
    /// decision is unambiguous.
    pub(crate) fn read(&self, after: u64, max: usize) -> Batch {
        // The earliest hole after the cursor wins: everything up to it is
        // unknown, so returning events would be a guess. Resuming at the hole's
        // own sequence number accepts the loss and continues past it.
        if let Some((seq, reason)) = self.gaps.iter().find(|(seq, _)| *seq > after) {
            return Batch {
                events: Vec::new(),
                next_seq: *seq,
                gap: Some(Gap {
                    first_missing: *seq,
                    reason: *reason,
                }),
                closed: self.closed,
            };
        }

        // A cursor older than the window lost coverage to retention. The extent
        // of the loss is unknown (`after + 1` is the first cursor we cannot
        // vouch for), and replay resumes at the first retained event.
        if let Some(first) = self.events.front().map(|event| event.seq)
            && self.dropped
            && after + 1 < first
        {
            return Batch {
                events: Vec::new(),
                // The last cursor we can vouch for: replay continues at the
                // window's first event, which `read` then delivers.
                next_seq: first.saturating_sub(1),
                gap: Some(Gap {
                    first_missing: after + 1,
                    reason: GapReason::Retention,
                }),
                closed: self.closed,
            };
        }

        let mut events = Vec::new();
        let mut next_seq = after;
        for event in &self.events {
            if event.seq <= after {
                continue;
            }
            if events.len() >= max.max(1) {
                break;
            }
            next_seq = event.seq;
            events.push(event.clone());
        }
        Batch {
            events,
            next_seq,
            gap: None,
            closed: self.closed,
        }
    }

    /// How many events are retained.
    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.events.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ring(retention: usize, pushes: usize) -> Ring {
        let mut ring = Ring::new(retention);
        for index in 0..pushes {
            ring.push(
                FileOp::Create,
                format!("f{index}"),
                None,
                false,
                index as u64,
                None,
                None,
                None,
            );
        }
        ring
    }

    #[test]
    fn a_fresh_read_returns_everything_in_order() {
        let ring = ring(8, 3);
        let batch = ring.read(0, 16);
        assert_eq!(batch.gap, None);
        assert_eq!(batch.events.len(), 3);
        assert_eq!(batch.events[0].seq, 1);
        assert_eq!(batch.events[2].seq, 3);
        assert_eq!(batch.next_seq, 3, "the cursor is the last delivered seq");
        assert!(!batch.closed);
    }

    #[test]
    fn a_cursor_resumes_exactly_after_the_last_event() {
        let ring = ring(8, 3);
        let batch = ring.read(2, 16);
        assert_eq!(batch.events.len(), 1);
        assert_eq!(batch.events[0].seq, 3);
        assert_eq!(batch.next_seq, 3);
        let again = ring.read(batch.next_seq, 16);
        assert!(again.events.is_empty());
        assert_eq!(again.gap, None);
        assert_eq!(again.next_seq, batch.next_seq, "an idle read does not move");
    }

    #[test]
    fn max_bounds_one_batch_and_the_cursor_continues() {
        let ring = ring(8, 5);
        let first = ring.read(0, 2);
        assert_eq!(first.events.len(), 2);
        assert_eq!(first.next_seq, 2);
        let second = ring.read(first.next_seq, 2);
        assert_eq!(second.events.len(), 2);
        assert_eq!(second.events[0].seq, 3);
        assert_eq!(second.next_seq, 4);
    }

    #[test]
    fn retention_loss_is_reported_as_a_gap_not_a_hole() {
        let ring = ring(2, 4);
        // Events 1 and 2 were dropped; the window holds 3 and 4.
        let batch = ring.read(0, 16);
        let gap = batch.gap.expect("retention gap");
        assert_eq!(gap.reason, GapReason::Retention);
        assert_eq!(
            gap.first_missing, 1,
            "coverage is unknown from the first unseen cursor"
        );
        assert_eq!(
            batch.next_seq, 2,
            "the last vouched-for cursor, so replay delivers seq 3 first"
        );
        assert!(batch.events.is_empty());

        // Resuming from the declared cursor delivers the retained events.
        let resumed = ring.read(batch.next_seq, 16);
        assert_eq!(resumed.gap, None);
        assert_eq!(resumed.events.len(), 2);
        assert_eq!(resumed.events[0].seq, 3);
    }

    #[test]
    fn a_source_gap_freezes_the_window_until_it_is_read() {
        let mut ring = ring(8, 2);
        ring.gap(GapReason::Overflow);
        let batch = ring.read(0, 16);
        let gap = batch.gap.expect("overflow gap");
        assert_eq!(gap.reason, GapReason::Overflow);
        assert_eq!(gap.first_missing, 3);
        // Resume after the hole: the ring is empty, so nothing follows.
        let resumed = ring.read(batch.next_seq, 16);
        assert_eq!(resumed.gap, None);
        assert!(resumed.events.is_empty());
    }

    #[test]
    fn a_gap_behind_the_cursor_does_not_reappear() {
        let mut ring = ring(8, 2);
        ring.gap(GapReason::Overflow);
        let batch = ring.read(0, 16);
        let resumed = ring.read(batch.next_seq, 16);
        assert_eq!(resumed.gap, None);
        // New events after the hole are visible.
        ring.push(
            FileOp::Create,
            "after".to_owned(),
            None,
            false,
            0,
            None,
            None,
            None,
        );
        let later = ring.read(resumed.next_seq, 16);
        assert_eq!(later.events.len(), 1);
        assert_eq!(later.events[0].path, "after");
    }

    #[test]
    fn closing_keeps_the_residue_readable() {
        let mut ring = ring(8, 2);
        ring.close();
        assert!(ring.read(0, 16).closed);
        let batch = ring.read(0, 16);
        assert_eq!(batch.events.len(), 2, "the residue stays readable");
    }

    #[test]
    fn nothing_is_appended_after_close() {
        let mut ring = ring(8, 1);
        ring.close();
        ring.push(
            FileOp::Create,
            "late".to_owned(),
            None,
            false,
            0,
            None,
            None,
            None,
        );
        assert_eq!(ring.len(), 1);
    }
}
