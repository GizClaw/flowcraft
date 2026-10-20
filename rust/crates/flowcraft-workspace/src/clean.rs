//! The lexical half: what a workspace path *means* before anything touches a
//! filesystem.
//!
//! core spells this with `path/filepath`; the port spells it with the `path`
//! rules -- slash-separated, no volume name, no host separator -- because a
//! workspace path is a portable namespace (see the crate docs). [`clean`] is
//! Go's `path.Clean`, ported rule for rule; [`dir`], [`base`] and [`join`] are
//! its three companions, because `atomic_write` needs a temp file next to its
//! target and `walk` builds child paths.
//!
//! [`clean_path`] is the one that decides access: it is the lexical half of
//! "paths are relative to the root", and every method of every backend calls it
//! before doing anything. It is deliberately *stricter* than a component-wise
//! check: core tests `strings.HasPrefix(cleaned, "..")`, so `..a` and `..foo/bar`
//! are refused as well. That is not an accident to be tidied up -- a name that
//! starts with two dots is a name an operator can create, and letting it through
//! would make the refusal depend on what the medium happens to contain -- so the
//! port reproduces it and the contract has rows for it.

use crate::Result;
use crate::refusal;

/// Returns `path` in its canonical lexical form, as Go's `path.Clean` does.
///
/// An empty path answers `"."`; repeated slashes collapse; `.` elements are
/// dropped; `..` elements consume the preceding element or are kept when there
/// is nothing to consume. The result is never empty.
pub fn clean(path: &str) -> String {
    if path.is_empty() {
        return ".".to_string();
    }
    let bytes = path.as_bytes();
    let rooted = bytes[0] == b'/';
    let n = bytes.len();

    let mut out: Vec<u8> = Vec::with_capacity(n);
    let mut r = 0usize;
    // Everything before `dotdot` is a prefix that `..` must not pop.
    let mut dotdot = 0usize;
    if rooted {
        out.push(b'/');
        r = 1;
        dotdot = 1;
    }

    while r < n {
        if bytes[r] == b'/' {
            // An empty element.
            r += 1;
        } else if bytes[r] == b'.' && (r + 1 == n || bytes[r + 1] == b'/') {
            // A `.` element.
            r += 1;
        } else if bytes[r] == b'.'
            && bytes.get(r + 1) == Some(&b'.')
            && (r + 2 == n || bytes[r + 2] == b'/')
        {
            // A `..` element: drop the last element, or keep the `..` when
            // there is nothing to drop and the path is relative.
            r += 2;
            if out.len() > dotdot {
                out.pop();
                while out.len() > dotdot && out.last() != Some(&b'/') {
                    out.pop();
                }
            } else if !rooted {
                if !out.is_empty() {
                    out.push(b'/');
                }
                out.push(b'.');
                out.push(b'.');
                dotdot = out.len();
            }
        } else {
            // A real element.
            if (rooted && out.len() != 1) || (!rooted && !out.is_empty()) {
                out.push(b'/');
            }
            while r < n && bytes[r] != b'/' {
                out.push(bytes[r]);
                r += 1;
            }
        }
    }

    if out.is_empty() {
        return ".".to_string();
    }
    match String::from_utf8(out) {
        Ok(cleaned) => cleaned,
        // Cleaning only drops bytes, and `path` was `&str`, so the remainder is
        // still UTF-8; a failure here would be a bug in this function.
        Err(err) => panic!("cleaning a UTF-8 path cannot change its bytes: {err}"),
    }
}

/// Validates `path` and answers its form relative to the view's root.
///
/// `Ok(None)` means the root itself (`""` and `"."`). An absolute path, or one
/// whose cleaned form begins with `..`, is the path-traversal refusal, whose
/// sentence carries the path *as given* -- the text an operator sees should be
/// what they wrote, not what it cleaned to.
pub fn clean_path(path: &str) -> Result<Option<String>> {
    if path.is_empty() || path == "." {
        return Ok(None);
    }
    let cleaned = clean(path);
    if cleaned.starts_with('/') || cleaned.starts_with("..") {
        return Err(refusal::path_traversal(path));
    }
    Ok(Some(cleaned))
}

/// The directory part of `path`, as Go's `path.Dir` answers it.
pub fn dir(path: &str) -> String {
    let split = match path.rfind('/') {
        Some(i) => &path[..=i],
        None => "",
    };
    clean(split)
}

/// The final element of `path`, as Go's `path.Base` answers it.
///
/// `""` answers `"."`; a path of only slashes answers `"/"`.
pub fn base(path: &str) -> String {
    if path.is_empty() {
        return ".".to_string();
    }
    let mut path = path;
    while let Some(stripped) = path.strip_suffix('/') {
        path = stripped;
    }
    let path = match path.rfind('/') {
        Some(i) => &path[i + 1..],
        None => path,
    };
    if path.is_empty() {
        return "/".to_string();
    }
    path.to_string()
}

/// Joins `elems` with `/` and cleans the result, as Go's `path.Join` does.
///
/// Empty elements are skipped, and an all-empty list answers `""` rather than
/// `"."` -- which is what lets [`crate::atomic_write`] build a temp name without
/// a leading `./`.
pub fn join(elems: &[&str]) -> String {
    let mut buf = String::new();
    for elem in elems {
        if !buf.is_empty() || !elem.is_empty() {
            if !buf.is_empty() {
                buf.push('/');
            }
            buf.push_str(elem);
        }
    }
    if buf.is_empty() {
        return String::new();
    }
    clean(&buf)
}

/// Returns `path` with case folded when the target filesystem folds it.
///
/// On the port's targets -- case-sensitive filesystems -- this is the identity,
/// and it is a function rather than an absence so that every call site that
/// compares paths goes through one name. core's answer on Windows is
/// `strings.ToLower` (a simple mapping); reproducing that here would be dead
/// code that claims a guarantee this crate does not test.
pub fn fold_case(path: &str) -> String {
    path.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{ok, refusal};

    #[test]
    fn clean_is_paths_clean() {
        assert_eq!(clean(""), ".");
        assert_eq!(clean("."), ".");
        assert_eq!(clean("./"), ".");
        assert_eq!(clean("./a"), "a");
        assert_eq!(clean("a/"), "a");
        assert_eq!(clean("a//b"), "a/b");
        assert_eq!(clean("a/./b"), "a/b");
        assert_eq!(clean("a/../b"), "b");
        assert_eq!(clean("a/b/../../c"), "c");
        assert_eq!(clean("a/.."), ".");
        assert_eq!(clean(".."), "..");
        assert_eq!(clean("../a"), "../a");
        assert_eq!(clean("a/../.."), "..");
        assert_eq!(clean("/"), "/");
        assert_eq!(clean("//a"), "/a");
        assert_eq!(clean("/a/../b"), "/b");
        assert_eq!(clean("/../a"), "/a");
        assert_eq!(clean("a/../../b"), "../b");
    }

    #[test]
    fn clean_path_is_stricter_than_a_component_check() {
        assert_eq!(ok(clean_path("")), None);
        assert_eq!(ok(clean_path(".")), None);
        // `./` is *not* the root to core: only the exact spellings `""` and `.`
        // are special-cased before `Clean`, and `Clean("./")` is `"."`.
        assert_eq!(ok(clean_path("./")).as_deref(), Some("."));
        assert_eq!(ok(clean_path("a/b")).as_deref(), Some("a/b"));
        assert_eq!(ok(clean_path("a/../b")).as_deref(), Some("b"));
        assert_eq!(ok(clean_path(".hidden")).as_deref(), Some(".hidden"));
        // The refusal core makes: `..` prefix, not `..` component.
        assert!(clean_path("..").is_err());
        assert!(clean_path("../a").is_err());
        assert!(clean_path("..a").is_err());
        assert!(clean_path("a/../../b").is_err());
        assert!(clean_path("/abs").is_err());
    }

    #[test]
    fn the_refusal_carries_the_path_as_given() {
        let err = refusal(clean_path("../a/./b"));
        assert_eq!(
            err.to_string(),
            "workspace: path traversal denied: ../a/./b"
        );
    }

    #[test]
    fn dir_and_base_agree_with_go() {
        assert_eq!(dir("a/b"), "a");
        assert_eq!(dir("a"), ".");
        assert_eq!(dir(""), ".");
        assert_eq!(dir("/a"), "/");
        assert_eq!(dir("a/b/"), "a/b");
        assert_eq!(dir("/"), "/");

        assert_eq!(base(""), ".");
        assert_eq!(base("a"), "a");
        assert_eq!(base("a/b"), "b");
        assert_eq!(base("a/b/"), "b");
        assert_eq!(base("/"), "/");
        assert_eq!(base("//"), "/");
    }

    #[test]
    fn join_is_clean_and_skips_empties() {
        assert_eq!(join(&[]), "");
        assert_eq!(join(&["", ""]), "");
        assert_eq!(join(&[".", "a"]), "a");
        assert_eq!(join(&["a", "b"]), "a/b");
        assert_eq!(join(&["a/", "/b"]), "a/b");
        assert_eq!(join(&["a", "", "b"]), "a/b");
    }
}
