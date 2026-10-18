//! `core/utils/pathsafe`: the containment rule -- "is this path the root or a
//! descendant of it?" -- in one place.
//!
//! It exists because the same check was re-implemented across core with
//! different strengths (a sandbox resolving a working directory, a settings
//! loader validating a relative path, a workspace classifying a resolved root),
//! and a missing symlink pass in any one of them is a path-escape bug.
//!
//! Two strengths, and callers must pick deliberately:
//!
//! - [`within`] / [`rel`] / [`resolve_under`] judge the paths as given,
//!   lexically. Right when the path is already resolved, or when the caller
//!   owns the resolution step.
//! - [`real_within`] / [`real_dir`] resolve symlinks first. Use them when an
//!   untrusted path is compared against a root: without the symlink pass, a
//!   link inside the root can point outside it.
//!
//! The module owns no I/O policy. Which roots exist, what is readable, and what
//! happens on a violation stay with the caller.
//!
//! # Paths are strings, and they are Unix paths
//!
//! core's functions take Go strings and go through `path/filepath`; the port
//! takes `&Path` and compares *components*, which is what `filepath.Rel` does
//! on Unix. The Windows spellings (`\x` being relative yet rooted,
//! `filepath.VolumeName`) are not here: Windows is its own slice
//! (`docs/plans/2026-10-15-rust-core-port.md` §12), and inventing them from a
//! Unix table would be the kind of quiet divergence this module exists to
//! prevent. [`rel_ref`] keeps core's rooted-prefix check, so the guard that
//! matters on Windows is a one-line addition, not a rewrite.
//!
//! The comparison is byte-wise, like Go's: two paths differing only in case are
//! different paths. [`within_fold`] is the case-insensitive variant, and it is
//! a separate function because the choice belongs to the caller.

use std::io;
use std::path::{Component, Path, PathBuf};

use flowcraft_contract::text;

/// The refusals this module makes.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// [`resolve_under`] was given an empty root.
    EmptyRoot,
    /// A relative reference climbed out of the root; the message names both.
    Escapes {
        /// The reference as the caller wrote it.
        reference: String,
        /// The root it was joined onto.
        root: String,
    },
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::EmptyRoot => f.write_str("pathsafe: empty root"),
            Error::Escapes { reference, root } => {
                write!(f, "pathsafe: {} escapes {root}", text::quote(reference))
            }
        }
    }
}

impl std::error::Error for Error {}

/// Whether `path` is `root` itself or a descendant of it.
///
/// The comparison is lexical: both arguments must be in the same form
/// (absolute, or relative to the same base) for the answer to mean anything. An
/// empty root contains nothing.
pub fn within(root: impl AsRef<Path>, path: impl AsRef<Path>) -> bool {
    let (root, path) = (root.as_ref(), path.as_ref());
    if root.as_os_str().is_empty() || path.as_os_str().is_empty() {
        return false;
    }
    if path == root {
        return true;
    }
    match walk(root, path) {
        Some(relative) => !escapes(&relative),
        None => false,
    }
}

/// [`within`] with case-insensitive comparison, for filesystems where case does
/// not distinguish names.
pub fn within_fold(root: impl AsRef<Path>, path: impl AsRef<Path>) -> bool {
    let (root, path) = (root.as_ref(), path.as_ref());
    if root.as_os_str().is_empty() || path.as_os_str().is_empty() {
        return false;
    }
    if text::equal_fold(&root.to_string_lossy(), &path.to_string_lossy()) {
        return true;
    }
    within(
        root.to_string_lossy().to_lowercase(),
        path.to_string_lossy().to_lowercase(),
    )
}

/// `target` expressed relative to `root`, or `None` when it is outside it (or
/// the two are not comparable). `root` itself yields `"."`.
///
/// The walk is `filepath.Rel` -- up for every component the root has beyond the
/// common prefix, then down -- and the answer is then held to the containment
/// rule, so a target that would come back as `"../x"` is `None` and not a
/// relative path that escapes. The contract rows pin both halves: `/w/root` to
/// `/w/other` is refused here, while `filepath.Rel` answers `"../other"`.
pub fn rel(root: impl AsRef<Path>, target: impl AsRef<Path>) -> Option<String> {
    let relative = walk(root, target)?;
    if escapes(&relative) {
        return None;
    }
    Some(relative)
}

/// `filepath.Rel` itself, before the containment rule.
fn walk(root: impl AsRef<Path>, target: impl AsRef<Path>) -> Option<String> {
    let (root, target) = (root.as_ref(), target.as_ref());
    if root.as_os_str().is_empty() || target.as_os_str().is_empty() {
        return None;
    }
    let root = clean(root);
    let target = clean(target);
    if root == target {
        return Some(".".to_owned());
    }
    let root_absolute = root.is_absolute();
    if root_absolute != target.is_absolute() {
        // filepath.Rel's "can't make ... relative to ..." error.
        return None;
    }
    let root_parts = components(&root);
    let target_parts = components(&target);
    let common = root_parts
        .iter()
        .zip(target_parts.iter())
        .take_while(|(left, right)| left == right)
        .count();
    let mut relative: Vec<String> = Vec::new();
    for _ in common..root_parts.len() {
        relative.push("..".to_owned());
    }
    for part in &target_parts[common..] {
        relative.push(part.clone());
    }
    if relative.is_empty() {
        return Some(".".to_owned());
    }
    Some(relative.join("/"))
}

/// Join one slash-separated relative reference onto `root` and refuse anything
/// that lands outside it, so a `..` component cannot walk out.
///
/// The result is cleaned, not symlink-resolved: a caller comparing it against
/// untrusted input wants [`real_within`] or its own resolution step.
pub fn resolve_under(root: impl AsRef<Path>, reference: &str) -> Result<PathBuf, Error> {
    let root = root.as_ref();
    if root.as_os_str().is_empty() {
        return Err(Error::EmptyRoot);
    }
    let clean_root = clean(root);
    // `filepath.Join` appends even an absolute reference -- it is not
    // `Path::join`, which would replace the base -- and lets the clean sort it
    // out. So a leading separator is dropped here and the rest is a relative
    // reference; `/absolute` lands inside the root as `absolute`, where a
    // `Path::join` would have silently answered `/absolute` and the
    // containment check below would have refused a reference core accepts.
    let joined = clean(clean_root.join(reference.trim_start_matches('/')));
    if !within(&clean_root, &joined) {
        return Err(Error::Escapes {
            reference: reference.to_owned(),
            root: root.to_string_lossy().into_owned(),
        });
    }
    Ok(joined)
}

/// Whether `reference` is a relative reference that cannot walk out of the
/// directory it will be joined onto: not absolute, not rooted, not `..`, and
/// not starting with `../`.
///
/// The sibling of [`within`] for names chosen by someone else -- a zip entry, a
/// manifest-declared binary, a plugin asset path -- before they are joined onto
/// a root. An empty reference cleans to `"."` and reports `true`; callers that
/// need a non-empty reference check that themselves.
pub fn rel_ref(reference: &str) -> bool {
    if reference.is_empty() {
        return true;
    }
    let cleaned = clean(Path::new(reference));
    let text = cleaned.to_string_lossy();
    !cleaned.is_absolute() && text != ".." && !text.starts_with("../")
}

/// Whether `path` is `root` or a descendant of it *after* resolving symlinks on
/// both sides.
///
/// A side that does not exist yet is resolved through its longest existing
/// ancestor, so it still compares against the resolved root (on macOS `/var` is
/// `/private/var`, and comparing a resolved root against an unresolved child
/// would wrongly read as "outside"). A caller that must fail closed on a path it
/// cannot resolve resolves it itself and uses [`within`].
pub fn real_within(root: impl AsRef<Path>, path: impl AsRef<Path>) -> bool {
    let (root, path) = (root.as_ref(), path.as_ref());
    if root.as_os_str().is_empty() || path.as_os_str().is_empty() {
        return false;
    }
    within(resolved_or_cleaned(root), resolved_or_cleaned(path))
}

/// Whether `path` exists, is a directory, and is not a symlink.
///
/// The guard for "may I move or adopt this tree?": following a link would move
/// whatever it points at. A path that does not exist is `Ok(false)`, matching
/// core; any other I/O failure is the caller's to see.
pub fn real_dir(path: impl AsRef<Path>) -> io::Result<bool> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) => Ok(metadata.is_dir() && !metadata.file_type().is_symlink()),
        Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(err) => Err(err),
    }
}

/// `path` with its symlinks resolved; a path that does not exist (or a relative
/// one that cannot be resolved) keeps its cleaned form.
fn resolved_or_cleaned(path: &Path) -> PathBuf {
    if let Ok(real) = std::fs::canonicalize(path) {
        return real;
    }
    let cleaned = clean(path);
    if !cleaned.is_absolute() {
        return cleaned;
    }
    match cleaned.parent() {
        // `parent()` of "/" is None: there is nothing left to resolve.
        None => cleaned,
        Some(parent) if parent.as_os_str().is_empty() => cleaned,
        Some(parent) => {
            let name = cleaned.file_name().unwrap_or_default();
            resolved_or_cleaned(parent).join(name)
        }
    }
}

/// `filepath.Clean`: collapse separators and `.`, fold inner `..` with the
/// element before it, and keep a relative leading `..` (an absolute one stops at
/// the root).
pub fn clean(path: impl AsRef<Path>) -> PathBuf {
    let path = path.as_ref();
    let absolute = path.is_absolute();
    let mut parts: Vec<Component<'_>> = Vec::new();
    let mut climbed = 0usize;
    for component in path.components() {
        match component {
            Component::RootDir | Component::Prefix(_) => {}
            Component::CurDir => {}
            Component::ParentDir => {
                if parts.len() > climbed {
                    parts.pop();
                } else if !absolute {
                    parts.push(Component::ParentDir);
                    climbed += 1;
                }
            }
            Component::Normal(name) => parts.push(Component::Normal(name)),
        }
    }
    let mut cleaned = PathBuf::new();
    if absolute {
        cleaned.push("/");
    }
    for part in parts {
        cleaned.push(part.as_os_str());
    }
    if cleaned.as_os_str().is_empty() {
        cleaned.push(".");
    }
    cleaned
}

/// The components of a cleaned path, as lossy strings for joining.
///
/// A leading `..` is a component like any other: dropping it would let
/// `rel("a/b", "../a/b/c")` line up the `a/b` on both sides and answer `"c"`,
/// where `filepath.Rel` walks off the top and answers `"../../../a/b/c"` -- and
/// the containment rule then refuses the row.
///
/// A `.` is *not* a component: after cleaning it can only be the whole path,
/// and `filepath.Rel` blanks it out before walking, which is why
/// `rel(".", "a")` is `"a"` rather than `"../a"`.
fn components(path: &Path) -> Vec<String> {
    path.components()
        .filter_map(|component| match component {
            Component::Normal(name) => Some(name.to_string_lossy().into_owned()),
            Component::ParentDir => Some("..".to_owned()),
            Component::RootDir => Some("/".to_owned()),
            Component::Prefix(prefix) => Some(prefix.as_os_str().to_string_lossy().into_owned()),
            Component::CurDir => None,
        })
        .collect()
}

/// Whether one [`rel`] result leaves the base: `".."` itself, or anything
/// starting with `"../"`.
fn escapes(relative: &str) -> bool {
    relative == ".." || relative.starts_with("../")
}

#[cfg(test)]
mod tests {
    use super::*;

    const ROOT: &str = "/w/root";

    #[test]
    fn within_is_lexical() {
        // core's table, one case at a time.
        for (label, path, expected) in [
            ("root itself", "/w/root", true),
            ("direct child", "/w/root/a", true),
            ("nested child", "/w/root/a/b/c", true),
            ("parent", "/w", false),
            ("sibling", "/w/rooted", false),
            ("uncleaned escape", "/w/root/../other", false),
            ("empty path", "", false),
        ] {
            assert_eq!(within(ROOT, path), expected, "{label}: {path}");
        }
    }

    #[test]
    fn within_fold_ignores_case_only() {
        for (label, path, expected) in [
            ("exact case", "/w/root", true),
            ("folded root itself", "/W/ROOT", true),
            ("folded child", "/W/root/sub", true),
            ("folded sibling", "/W/rooted", false),
            ("folded parent", "/W", false),
            ("empty path", "", false),
        ] {
            assert_eq!(within_fold(ROOT, path), expected, "{label}: {path}");
        }
    }

    #[test]
    fn rel_walks_up_and_down() {
        for (label, target, expected) in [
            ("root itself", "/w/root", Some(".")),
            ("child", "/w/root/a", Some("a")),
            ("nested", "/w/root/a/b", Some("a/b")),
            ("outside", "/w/other", None),
            ("uncleaned", "/w/root/a/../b", Some("b")),
            ("absolute against relative", "relative/path", None),
        ] {
            assert_eq!(rel(ROOT, target).as_deref(), expected, "{label}");
        }
    }

    #[test]
    fn resolve_under_refuses_an_escape() {
        assert_eq!(
            resolve_under(ROOT, "sub/file.txt"),
            Ok(PathBuf::from("/w/root/sub/file.txt"))
        );
        assert_eq!(resolve_under(ROOT, "."), Ok(PathBuf::from("/w/root")));
        assert_eq!(
            resolve_under(ROOT, "a/b/c"),
            Ok(PathBuf::from("/w/root/a/b/c"))
        );
        // `/absolute` is *not* an escape: filepath.Join appends it, so it lands
        // inside the root -- core's own answer, and one a `Path::join` port
        // gets wrong in the refusing direction.
        assert_eq!(
            resolve_under(ROOT, "/absolute"),
            Ok(PathBuf::from("/w/root/absolute"))
        );
        for escape in ["../escape", "sub/../../escape", ".."] {
            let error = match resolve_under(ROOT, escape) {
                Ok(path) => panic!("{escape} resolved to {path:?}"),
                Err(error) => error,
            };
            assert!(matches!(error, Error::Escapes { .. }), "{escape}: {error}");
        }
        assert_eq!(resolve_under("", "x"), Err(Error::EmptyRoot));
    }

    #[test]
    fn rel_ref_rejects_anything_that_climbs() {
        for (reference, expected) in [
            ("bin/plugin", true),
            ("./bin/plugin", true),
            ("a/../b", true),
            ("", true),
            ("..", false),
            ("../escape", false),
            ("a/../../escape", false),
            ("/absolute", false),
        ] {
            assert_eq!(rel_ref(reference), expected, "{reference:?}");
        }
    }

    #[test]
    fn clean_matches_filepaths_answers() {
        for (input, expected) in [
            ("/w/root/", "/w/root"),
            ("/w//root/./a", "/w/root/a"),
            ("/w/root/../other", "/w/other"),
            ("/w/root/a/..", "/w/root"),
            ("/..", "/"),
            ("", "."),
            (".", "."),
            ("a/../../b", "../b"),
            ("../../a", "../../a"),
        ] {
            assert_eq!(clean(input), PathBuf::from(expected), "{input:?}");
        }
    }

    #[test]
    fn real_dir_sees_through_a_link() -> io::Result<()> {
        let base = std::env::temp_dir().join(format!("pathsafe-{}", std::process::id()));
        let real = base.join("real");
        let link = base.join("link");
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&real)?;
        std::os::unix::fs::symlink(&real, &link)?;

        assert_eq!(real_dir(&real).ok(), Some(true));
        assert_eq!(
            real_dir(&link).ok(),
            Some(false),
            "a link is not a real dir"
        );
        assert_eq!(real_dir(base.join("absent")).ok(), Some(false));
        assert!(real_within(&base, &real));
        assert!(
            real_within(&base, &link),
            "a link inside the root resolves inside it"
        );
        let _ = std::fs::remove_dir_all(&base);
        Ok(())
    }

    #[test]
    fn real_within_resolves_a_link_that_leaves_the_root() -> io::Result<()> {
        let base = std::env::temp_dir().join(format!("pathsafe-escape-{}", std::process::id()));
        let inside = base.join("inside");
        let outside = base.join("outside");
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&inside)?;
        std::fs::create_dir_all(&outside)?;
        let link = inside.join("escape");
        std::os::unix::fs::symlink(&outside, &link)?;

        assert!(
            within(&inside, &link),
            "lexically the link is inside the root"
        );
        assert!(
            !real_within(&inside, &link),
            "resolved, it is not -- which is why the symlink pass exists"
        );
        assert!(
            !real_within(&inside, link.join("new-file")),
            "a file that does not exist yet resolves through its ancestor, \
             and this one's ancestor is outside the root"
        );
        std::fs::create_dir_all(inside.join("sub"))?;
        assert!(
            real_within(&inside, inside.join("sub/new-file")),
            "the same rule, on an ancestor that is inside the root"
        );
        let _ = std::fs::remove_dir_all(&base);
        Ok(())
    }
}
