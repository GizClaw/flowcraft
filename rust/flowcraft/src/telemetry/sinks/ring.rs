//! A bounded in-memory buffer of the most recent output.
//!
//! This is what makes a client-side daemon diagnosable without writing
//! everything to disk: the ring always holds the last few hundred kilobytes,
//! and a support bundle can ask for them on demand. When it overflows the
//! oldest bytes go, which is the right trade — a report is about what just
//! happened.

use std::collections::VecDeque;
use std::sync::Mutex;

/// The default bound: enough for a few thousand records, small enough that a
/// process which is not being diagnosed costs nothing noticeable.
pub const DEFAULT_BYTES: usize = 256 * 1024;

/// A shared ring buffer.
#[derive(Debug)]
pub struct Shared {
    buffer: Mutex<VecDeque<u8>>,
    capacity: usize,
}

impl Shared {
    /// A ring holding at most `capacity` bytes.
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self {
            buffer: Mutex::new(VecDeque::with_capacity(capacity.min(DEFAULT_BYTES))),
            capacity,
        }
    }

    /// The bound in bytes.
    #[must_use]
    pub fn capacity(&self) -> usize {
        self.capacity
    }

    /// Append `bytes`, dropping the oldest ones to stay inside the bound.
    ///
    /// A record longer than the whole ring keeps its tail: the newest bytes are
    /// the ones a reader needs.
    pub fn push(&self, bytes: &[u8]) {
        if self.capacity == 0 {
            return;
        }
        let Ok(mut buffer) = self.buffer.lock() else {
            return;
        };
        if bytes.len() >= self.capacity {
            buffer.clear();
            buffer.extend(&bytes[bytes.len() - self.capacity..]);
            return;
        }
        buffer.extend(bytes);
        let excess = buffer.len().saturating_sub(self.capacity);
        buffer.drain(..excess);
    }

    /// Everything held, lossily decoded.
    #[must_use]
    pub fn dump(&self) -> String {
        let Ok(buffer) = self.buffer.lock() else {
            return String::new();
        };
        String::from_utf8_lossy(&buffer.iter().copied().collect::<Vec<u8>>()).into_owned()
    }

    /// How many bytes are held.
    #[must_use]
    pub fn len(&self) -> usize {
        self.buffer.lock().map(|buffer| buffer.len()).unwrap_or(0)
    }

    /// Whether the ring is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keeps_the_newest_bytes() {
        let ring = Shared::new(8);
        ring.push(b"0123456789");
        assert_eq!(ring.len(), 8);
        assert_eq!(ring.dump(), "23456789");
    }

    #[test]
    fn accumulates_records_in_order() {
        let ring = Shared::new(16);
        ring.push(b"one\n");
        ring.push(b"two\n");
        assert_eq!(ring.dump(), "one\ntwo\n");
    }

    #[test]
    fn an_oversized_record_keeps_its_tail() {
        let ring = Shared::new(4);
        ring.push(b"keep-me-please");
        assert_eq!(ring.dump(), "ease");
    }

    #[test]
    fn never_exceeds_its_capacity() {
        let ring = Shared::new(10);
        for _ in 0..100 {
            ring.push(b"0123456789abc");
        }
        assert_eq!(ring.len(), 10);
    }

    #[test]
    fn a_zero_capacity_ring_stays_empty() {
        let ring = Shared::new(0);
        ring.push(b"ignored");
        assert_eq!(ring.len(), 0);
        assert!(ring.is_empty());
    }
}
