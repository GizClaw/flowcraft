//! [`capped`]: one file, read under a byte cap, preferring the backend's own
//! bounded read.
//!
//! Wrapping [`Workspace::read`] with a limit is deliberately *not* the shape
//! here. A wrapper can only trim after the fact, and by then the bytes are
//! already in memory -- the point of bounds like the skill body cap is that an
//! oversized file never lands there at all. So the backend is asked first
//! ([`Workspace::as_limited`]), and the fallback for a backend that cannot bound
//! a read exists, but is a fallback rather than the rule.
//!
//! The two paths answer *different sentences* when the ceiling is crossed, and
//! that is the contract rather than an accident:
//!
//! - a bounded read says `workspace: <path> exceeds <n> bytes`;
//! - the fallback says `workspace: <path> is <n> bytes, over the <n>-byte read
//!   cap`.
//!
//! A reader can therefore tell whether a backend materialized the file before
//! refusing it. There are rows for both.

use crate::refusal;
use crate::{Result, Workspace};

/// Reads `path` under a cap of `max` bytes.
///
/// A non-positive cap is a validation refusal (core's rule: a caller that asks
/// for a zero-byte read has a bug, and answering `""` hides it). A file exactly
/// at the cap is accepted.
pub fn capped(ws: &dyn Workspace, path: &str, max: i64) -> Result<Vec<u8>> {
    if max <= 0 {
        return Err(refusal::validation(format!(
            "workspace: read cap must be positive, got {max}"
        )));
    }

    if let Some(limited) = ws.as_limited() {
        let data = limited.read_limited(path, max)?;
        // Defensive, as in core: a backend that answers the bounded-read
        // interface and then ignores the cap must still not reach a decoder.
        if data.len() as i64 > max {
            return Err(over_cap(path, data.len(), max));
        }
        return Ok(data);
    }

    let data = ws.read(path)?;
    if data.len() as i64 > max {
        return Err(over_cap(path, data.len(), max));
    }
    Ok(data)
}

/// The fallback's sentence: the file was already in memory.
fn over_cap(path: &str, len: usize, max: i64) -> flowcraft_contract::error::Error {
    refusal::validation(format!(
        "workspace: {path} is {len} bytes, over the {max}-byte read cap"
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mem::MemWorkspace;
    use crate::testing::{ok, refusal, refused};

    #[test]
    fn the_fallback_names_the_size_it_read() {
        let ws = MemWorkspace::with_files([("a.txt", "0123456789")]);
        assert_eq!(ok(capped(&ws, "a.txt", 10)), b"0123456789");
        let err = refusal(capped(&ws, "a.txt", 9));
        assert_eq!(
            err.to_string(),
            "workspace: a.txt is 10 bytes, over the 9-byte read cap"
        );
    }

    #[test]
    fn a_non_positive_cap_is_a_refusal_not_an_empty_read() {
        let ws = MemWorkspace::with_files([("a.txt", "x")]);
        assert_eq!(
            refused(capped(&ws, "a.txt", 0)),
            "workspace: read cap must be positive, got 0"
        );
        assert_eq!(
            refused(capped(&ws, "a.txt", -1)),
            "workspace: read cap must be positive, got -1"
        );
    }

    #[test]
    fn a_missing_file_stays_not_found() {
        let ws = MemWorkspace::new();
        assert_eq!(
            refused(capped(&ws, "nope", 10)),
            "workspace: not found: nope"
        );
    }
}
