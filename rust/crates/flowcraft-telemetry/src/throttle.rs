//! Keeping a repeated failure from becoming a flood.
//!
//! An endpoint that denies a token, a craft that cannot bind, a disk that stays
//! full — each of those can emit the same line thousands of times a minute. On a
//! server that is a bill and a useless log; on a user's machine it is a full
//! disk. The record itself is worth keeping, so the first few in a window are
//! written and the rest are counted: the next record of that key carries the
//! count in `suppressed`, which is where a reader can still see the scale.
//!
//! Only `WARN` and `ERROR` are throttled. `INFO` and below are state
//! transitions or per-request detail; if those are too loud, the fix is the
//! filter, not a counter.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use tracing::Level;

use crate::format::Record;

/// More distinct keys than this and the table is reset.
///
/// A client cannot be allowed to grow the table with unique messages; losing the
/// counters is cheaper than losing the memory.
const MAX_KEYS: usize = 4096;

/// How many records of one key may be written per window.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Policy {
    /// Records per window. Zero disables throttling.
    pub per_window: u32,
    /// The window.
    pub window: Duration,
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            per_window: 60,
            window: Duration::from_secs(60),
        }
    }
}

impl Policy {
    /// Off.
    #[must_use]
    pub fn disabled() -> Self {
        Self {
            per_window: 0,
            window: Duration::from_secs(60),
        }
    }

    /// Whether this policy would ever drop anything.
    #[must_use]
    pub fn is_enabled(&self) -> bool {
        self.per_window > 0 && !self.window.is_zero()
    }
}

/// The counters behind one installed pipeline.
#[derive(Debug)]
pub struct Throttle {
    policy: Policy,
    keys: Mutex<HashMap<Key, Window>>,
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
struct Key {
    level: Level,
    target: String,
    message: String,
    kind: Option<String>,
}

#[derive(Debug)]
struct Window {
    start: Instant,
    written: u32,
    suppressed: u32,
}

impl Throttle {
    /// Counters for `policy`.
    #[must_use]
    pub fn new(policy: Policy) -> Self {
        Self {
            policy,
            keys: Mutex::new(HashMap::new()),
        }
    }

    /// Whether this record may be written, and how many were dropped before it.
    ///
    /// `Some(0)` for a record that needs no annotation; `None` when the record
    /// is a repeat inside the window.
    pub(crate) fn admit(&self, record: &Record) -> Option<u32> {
        if !self.policy.is_enabled() || !throttles(record.level) {
            return Some(0);
        }
        let key = Key {
            level: record.level,
            target: record.target.clone(),
            message: record.message.clone(),
            kind: field(record, crate::attrs::attr::ERROR_KIND),
        };
        let Ok(mut keys) = self.keys.lock() else {
            return Some(0);
        };
        if keys.len() > MAX_KEYS {
            keys.clear();
        }
        let policy = self.policy;
        let window = keys.entry(key).or_insert_with(|| Window {
            start: Instant::now(),
            written: 0,
            suppressed: 0,
        });
        if window.start.elapsed() >= policy.window {
            window.start = Instant::now();
            window.written = 0;
            // The count from the previous window rides on this record.
            let carried = std::mem::take(&mut window.suppressed);
            window.written = 1;
            return Some(carried);
        }
        if window.written < policy.per_window {
            window.written += 1;
            return Some(std::mem::take(&mut window.suppressed));
        }
        window.suppressed = window.suppressed.saturating_add(1);
        None
    }
}

fn throttles(level: Level) -> bool {
    matches!(level, Level::WARN | Level::ERROR)
}

fn field(record: &Record, name: &str) -> Option<String> {
    record
        .fields
        .iter()
        .find(|field| field.name == name)
        .map(|field| crate::format::render_scalar(&field.value))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::format::Value;

    fn record(level: Level, message: &str) -> Record {
        Record {
            at: std::time::SystemTime::now(),
            level,
            target: "flowcraft_platformd::hosting".to_owned(),
            message: message.to_owned(),
            trace: None,
            fields: Vec::new(),
        }
    }

    fn throttle(per_window: u32) -> Throttle {
        Throttle::new(Policy {
            per_window,
            window: Duration::from_secs(60),
        })
    }

    #[test]
    fn a_repeated_warning_is_capped_and_counted() {
        let throttle = throttle(2);
        let line = record(Level::WARN, "grant renewal failed");
        assert_eq!(throttle.admit(&line), Some(0));
        assert_eq!(throttle.admit(&line), Some(0));
        assert_eq!(throttle.admit(&line), None);
        assert_eq!(throttle.admit(&line), None);
    }

    #[test]
    fn different_messages_have_separate_budgets() {
        let throttle = throttle(1);
        assert_eq!(throttle.admit(&record(Level::WARN, "a")), Some(0));
        assert_eq!(throttle.admit(&record(Level::WARN, "a")), None);
        assert_eq!(throttle.admit(&record(Level::WARN, "b")), Some(0));
    }

    #[test]
    fn the_error_kind_is_part_of_the_key() {
        let throttle = throttle(1);
        let mut first = record(Level::ERROR, "reload failed");
        first.fields.push(crate::format::Field {
            name: crate::attrs::attr::ERROR_KIND.to_owned(),
            value: Value::Str("validation".to_owned()),
        });
        let mut second = first.clone();
        second.fields[0].value = Value::Str("timeout".to_owned());

        assert_eq!(throttle.admit(&first), Some(0));
        assert_eq!(throttle.admit(&first), None);
        // A different category is a different failure, and is reported.
        assert_eq!(throttle.admit(&second), Some(0));
    }

    #[test]
    fn informational_records_are_never_throttled() {
        let throttle = throttle(1);
        let line = record(Level::INFO, "same");
        for _ in 0..10 {
            assert_eq!(throttle.admit(&line), Some(0));
        }
    }

    #[test]
    fn a_zero_policy_throttles_nothing() {
        let throttle = Throttle::new(Policy::disabled());
        let line = record(Level::ERROR, "same");
        for _ in 0..10 {
            assert_eq!(throttle.admit(&line), Some(0));
        }
    }

    #[test]
    fn the_table_does_not_grow_without_bound() {
        let throttle = throttle(1);
        for index in 0..(MAX_KEYS + 10) {
            let _ = throttle.admit(&record(Level::WARN, &format!("message {index}")));
        }
        let size = throttle.keys.lock().map(|keys| keys.len()).unwrap_or(0);
        assert!(size <= MAX_KEYS + 1, "table grew to {size}");
    }
}
