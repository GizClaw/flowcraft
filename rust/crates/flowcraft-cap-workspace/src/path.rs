//! Path rules shared by every provider and view.
//!
//! One implementation, so a prefixed view ([`crate::sub`]) and a local provider
//! cannot disagree about what a path means. The rules match the Go side's
//! `cleanPath`, because a document, a test, or an operator moving between the
//! two trees should not have to learn a second spelling:
//!
//! * `""` and `"."` name the root of the view;
//! * `a/../b` is cleaned to `b` (interior `..` is allowed when it stays inside);
//! * `..` that would climb above the root, and any absolute path, is
//!   [`ErrorKind::Forbidden`] — the class Go's `ErrPathTraversal` carries.
//!
//! This is a lexical pass for clear errors and stable spelling. The security
//! boundary is the capability handle underneath (see [`crate::local`]), which
//! re-resolves every component at use time.
//!
//! [`ErrorKind::Forbidden`]: flowcraft_error::ErrorKind::Forbidden

use std::path::{Component, Path};

use flowcraft_error::{Error, Result};

/// Upper bound on one path, so a pathological string fails fast.
const MAX_PATH: usize = 4096;

/// Clean a caller path into the portable relative form.
///
/// Returns an empty string for the root of the view. Slash-separated, no `.`
/// segments, never absolute, never escaping.
///
/// # Errors
///
/// [`ErrorKind::Validation`](flowcraft_error::ErrorKind::Validation) when the
/// path is longer than 4096 bytes;
/// [`ErrorKind::Forbidden`](flowcraft_error::ErrorKind::Forbidden) when it is
/// absolute or escapes the root.
pub(crate) fn clean(path: &str) -> Result<String> {
    if path.len() > MAX_PATH {
        return Err(Error::validation(format!(
            "path is longer than {MAX_PATH} bytes"
        )));
    }
    if Path::new(path).is_absolute() {
        return Err(Error::forbidden(format!(
            "path {path:?} must be relative to the workspace root"
        )));
    }
    let mut parts: Vec<&std::ffi::OsStr> = Vec::new();
    for component in Path::new(path).components() {
        match component {
            Component::Normal(part) => parts.push(part),
            Component::CurDir => {}
            Component::ParentDir => {
                if parts.pop().is_none() {
                    return Err(Error::forbidden(format!(
                        "path {path:?} escapes the workspace root"
                    )));
                }
            }
            // `is_absolute` already rejected the rooted forms; a prefix that
            // slipped through (a Windows drive-relative path) is a refusal too,
            // never a silently reinterpreted path.
            Component::RootDir | Component::Prefix(_) => {
                return Err(Error::forbidden(format!(
                    "path {path:?} must be relative to the workspace root"
                )));
            }
        }
    }
    let mut cleaned = String::new();
    for part in parts {
        if !cleaned.is_empty() {
            cleaned.push('/');
        }
        cleaned.push_str(&part.to_string_lossy());
    }
    Ok(cleaned)
}

/// Join a view prefix and a cleaned child path with the portable separator.
pub(crate) fn join(prefix: &str, child: &str) -> String {
    match (prefix.is_empty(), child.is_empty()) {
        (true, _) => child.to_owned(),
        (false, true) => prefix.to_owned(),
        (false, false) => format!("{prefix}/{child}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_root_has_one_spelling() {
        assert_eq!(clean("").unwrap(), "");
        assert_eq!(clean(".").unwrap(), "");
        assert_eq!(clean("./").unwrap(), "");
    }

    #[test]
    fn interior_parent_segments_are_cleaned() {
        assert_eq!(clean("a/../b.txt").unwrap(), "b.txt");
        assert_eq!(clean("a/b/../../c").unwrap(), "c");
        assert_eq!(clean("./a//b").unwrap(), "a/b");
    }

    #[test]
    fn escapes_are_forbidden() {
        for path in ["..", "../a", "a/../../b", "/etc/passwd"] {
            assert_eq!(
                clean(path).unwrap_err().kind(),
                flowcraft_error::ErrorKind::Forbidden,
                "{path}"
            );
        }
    }

    #[test]
    fn an_overlong_path_is_a_validation_error() {
        let path = "a".repeat(MAX_PATH + 1);
        assert_eq!(
            clean(&path).unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }

    #[test]
    fn joining_keeps_one_separator() {
        assert_eq!(join("", "a/b"), "a/b");
        assert_eq!(join("run-1", "a/b"), "run-1/a/b");
        assert_eq!(join("run-1", ""), "run-1");
        assert_eq!(join("", ""), "");
    }
}
