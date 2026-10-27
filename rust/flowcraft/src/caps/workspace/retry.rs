//! Bounded retry for the failures Windows can produce and POSIX cannot.
//!
//! Windows refuses to remove a file another process holds open (an editor, a
//! virus scan, Explorer); POSIX `unlink` succeeds regardless. The refusal is
//! transient, so a short bounded retry turns a race into a success while a file
//! that stays locked still fails honestly instead of hanging.
//!
//! Everything except the entry-point selection is portable and tested on every
//! platform: the loop, and the classification of the two Windows codes. What
//! differs per platform is one decision — whether to spend the retries at all —
//! which mirrors the Go side's `removeWithRetry` in `remove_windows.go` and the
//! pass-through in `remove_other.go`.

// The loop and the classification exist for the Windows entry point, but the
// tests below exercise both on every platform; a non-Windows library build
// therefore sees them as unused, not as an error.
#![cfg_attr(not(windows), allow(dead_code))]

use std::io;
use std::time::Duration;

/// How many times a remove-shaped operation is attempted before the original
/// error is reported. Three attempts with a 200 ms then 400 ms pause: long
/// enough to outlast a scan, short enough that a truly locked file is reported
/// instead of waited on.
const ATTEMPTS: u32 = 3;

/// The pause after the first failure; each further failure doubles it.
const FIRST_DELAY: Duration = Duration::from_millis(200);

/// `ERROR_SHARING_VIOLATION` (32) and `ERROR_LOCK_VIOLATION` (33): the two codes
/// `remove_file` / `remove_dir_all` surface when the target is held open by
/// another process. They are the only failures worth a second attempt — a
/// missing path or a real permission error is final, and retrying it would only
/// delay the honest answer.
fn is_sharing_violation(error: &io::Error) -> bool {
    matches!(error.raw_os_error(), Some(32) | Some(33))
}

/// Run `op`, retrying while it fails with a transient error and attempts remain.
///
/// The last error is returned as it came from the filesystem, so a caller sees
/// what actually failed rather than a synthesized "gave up".
// A synchronous helper by contract: every caller is already on the blocking
// pool (or in assembly).
#[allow(clippy::disallowed_methods)]
pub(crate) fn retry_when<T, P, F>(attempts: u32, mut transient: P, mut op: F) -> io::Result<T>
where
    P: FnMut(&io::Error) -> bool,
    F: FnMut() -> io::Result<T>,
{
    let mut delay = FIRST_DELAY;
    let mut attempt = 1;
    loop {
        match op() {
            Ok(value) => return Ok(value),
            Err(error) if attempt < attempts && transient(&error) => {
                std::thread::sleep(delay);
                delay *= 2;
                attempt += 1;
            }
            Err(error) => return Err(error),
        }
    }
}

/// Run a remove-shaped operation, retrying the refusals Windows produces while
/// another process holds the target open.
#[cfg(windows)]
pub(crate) fn remove<T, F>(op: F) -> io::Result<T>
where
    F: FnMut() -> io::Result<T>,
{
    retry_when(ATTEMPTS, is_sharing_violation, op)
}

/// Run a remove-shaped operation. Unix unlinks an open file, so this is a
/// pass-through and the retry is never spent.
#[cfg(not(windows))]
pub(crate) fn remove<T, F>(mut op: F) -> io::Result<T>
where
    F: FnMut() -> io::Result<T>,
{
    op()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn transient(error: &io::Error) -> bool {
        error.kind() == io::ErrorKind::PermissionDenied
    }

    fn denied() -> io::Error {
        io::Error::from(io::ErrorKind::PermissionDenied)
    }

    #[test]
    fn a_transient_failure_is_retried_into_a_success() {
        let mut calls = 0;
        let value = retry_when(2, transient, || {
            calls += 1;
            if calls < 2 { Err(denied()) } else { Ok(7) }
        })
        .unwrap();
        assert_eq!(value, 7);
        assert_eq!(calls, 2);
    }

    #[test]
    fn the_original_error_survives_a_final_failure() {
        let mut calls = 0;
        let error = retry_when::<(), _, _>(2, transient, || {
            calls += 1;
            Err(denied())
        })
        .unwrap_err();
        assert_eq!(calls, 2);
        assert_eq!(error.kind(), io::ErrorKind::PermissionDenied);
    }

    #[test]
    fn a_persistent_failure_is_not_retried() {
        let mut calls = 0;
        let error = retry_when::<(), _, _>(3, transient, || {
            calls += 1;
            Err(io::Error::from(io::ErrorKind::NotFound))
        })
        .unwrap_err();
        assert_eq!(calls, 1);
        assert_eq!(error.kind(), io::ErrorKind::NotFound);
    }

    /// The classification is pure, so it is checkable off Windows too: the codes
    /// are numbers, not platform types.
    #[test]
    fn only_sharing_and_lock_violations_are_transient() {
        assert!(is_sharing_violation(&io::Error::from_raw_os_error(32)));
        assert!(is_sharing_violation(&io::Error::from_raw_os_error(33)));
        for code in [2, 5, 112, 1234] {
            assert!(
                !is_sharing_violation(&io::Error::from_raw_os_error(code)),
                "{code}"
            );
        }
        assert!(!is_sharing_violation(&denied()));
    }

    /// Only runs where the retry is armed: a sharing violation is what a remove
    /// retries, and the Windows CI lane proves it end to end.
    #[cfg(windows)]
    #[test]
    fn a_remove_retries_a_sharing_violation() {
        let mut calls = 0;
        let value = remove(|| {
            calls += 1;
            if calls < 3 {
                Err(io::Error::from_raw_os_error(32))
            } else {
                Ok("removed")
            }
        })
        .unwrap();
        assert_eq!(value, "removed");
        assert_eq!(calls, 3);
    }

    #[cfg(windows)]
    #[test]
    fn a_remove_does_not_retry_a_missing_path() {
        let mut calls = 0;
        let error = remove::<(), _>(|| {
            calls += 1;
            Err(io::Error::from_raw_os_error(2))
        })
        .unwrap_err();
        assert_eq!(calls, 1);
        assert_eq!(error.raw_os_error(), Some(2));
    }
}
