//! Root-relative path resolution.
//!
//! Three questions, answered in one place because every backend must answer
//! them identically:
//!
//! - what does a relative settings path mean (`root.join`), and is the result
//!   still inside the root *after* symlinks are resolved?
//! - what does a workdir mean (`root.join` for a relative one; an absolute one
//!   must already be inside the root)?
//! - which writable paths does the root itself conflict with?
//!
//! Containment is always checked twice: once lexically (so `../` cannot be
//! used) and once on the resolved real path (so a symlink inside the root
//! cannot point outside it). Only the second check needs the filesystem, so
//! [`within`] stays a pure function.

use std::path::{Component, Path, PathBuf};

use crate::error::{Error, Result};

/// Reject a path a shell, a profile, or a log line could be confused by.
///
/// A control character in a path is never legitimate here, and SBPL strings and
/// argv both lose information when one sneaks in — the profile builder can then
/// be talked into emitting a second rule. Rejecting it at resolution keeps that
/// escaping purely mechanical.
fn check_text(path: &Path) -> Result<()> {
    if path.to_string_lossy().chars().any(char::is_control) {
        return Err(Error::validation(format!(
            "sandbox: path {} contains a control character",
            path.display()
        )));
    }
    Ok(())
}

/// Resolve a configured root: absolute, canonical, and a directory.
pub(crate) fn resolve_root(raw: &str) -> Result<PathBuf> {
    if raw.trim().is_empty() {
        return Err(Error::validation("sandbox: settings.root is required"));
    }
    check_text(Path::new(raw))?;
    let path = PathBuf::from(raw);
    let absolute = if path.is_absolute() {
        path
    } else {
        std::env::current_dir()
            .map_err(|error| {
                Error::internal(format!("sandbox: resolve the working directory: {error}"))
            })?
            .join(path)
    };
    let real = std::fs::canonicalize(&absolute).map_err(|error| {
        Error::not_found(format!(
            "sandbox: root {} is not usable: {error}",
            absolute.display()
        ))
    })?;
    if !real.is_dir() {
        return Err(Error::validation(format!(
            "sandbox: root {} is not a directory",
            absolute.display()
        )));
    }
    check_text(&real)?;
    Ok(real)
}

/// Lexical containment, component by component.
///
/// `Path::starts_with` compares whole components, so `/srv/box2` is *not* inside
/// `/srv/box` — the string-prefix bug this function exists to avoid.
#[must_use]
pub(crate) fn within(root: &Path, path: &Path) -> bool {
    path.starts_with(root)
}

/// Canonicalize the longest existing prefix and re-append the rest.
///
/// A workdir that does not exist yet still resolves predictably; only the part
/// that exists can be checked for symlink escapes, which is exactly the part an
/// attacker would use.
pub(crate) fn resolve_existing_prefix(path: &Path) -> Result<PathBuf> {
    let mut missing: Vec<std::ffi::OsString> = Vec::new();
    let mut current = path.to_path_buf();
    loop {
        match std::fs::canonicalize(&current) {
            Ok(mut real) => {
                for part in missing.iter().rev() {
                    real.push(part);
                }
                return Ok(real);
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                let Some(name) = current.file_name().map(|name| name.to_os_string()) else {
                    // Reached the filesystem root: nothing above it exists.
                    return Ok(current);
                };
                missing.push(name);
                if !current.pop() {
                    return Ok(current);
                }
            }
            Err(error) => {
                return Err(Error::validation(format!(
                    "sandbox: resolve {}: {error}",
                    path.display()
                )));
            }
        }
    }
}

/// Remove `.` and `..` lexically, without touching the filesystem.
#[must_use]
pub(crate) fn normalize(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    out.push(component);
                }
            }
            other => out.push(other),
        }
    }
    out
}

fn traversal(path: &Path, detail: &str) -> Error {
    Error::forbidden(format!(
        "sandbox: path traversal denied: {} escapes the runner root ({detail})",
        path.display()
    ))
}

/// Canonicalize a root, so containment is compared between two real paths.
///
/// On macOS `/var` and `/tmp` are symlinks, so a root spelled with the symlinked
/// prefix would reject every resolved child path. Doing it here means a caller
/// that constructs a runner directly cannot end up with false refusals.
fn real_root(root: &Path) -> Result<PathBuf> {
    std::fs::canonicalize(root).map_err(|error| {
        Error::not_found(format!(
            "sandbox: root {} is not usable: {error}",
            root.display()
        ))
    })
}

/// Resolve a session's working directory against `root`.
///
/// A relative directory is joined to the root; an absolute one must already be
/// inside it. Both are then resolved through symlinks and checked again.
pub(crate) fn resolve_workdir(root: &Path, dir: Option<&Path>) -> Result<PathBuf> {
    let root = real_root(root)?;
    let root = root.as_path();
    let candidate = match dir {
        None => root.to_path_buf(),
        Some(dir) if dir.is_absolute() => dir.to_path_buf(),
        Some(dir) => root.join(dir),
    };
    check_text(&candidate)?;
    let clean = normalize(&candidate);
    if !within(root, &clean) {
        return Err(traversal(&clean, "lexically outside the root"));
    }
    let real = resolve_existing_prefix(&clean)?;
    if !within(root, &real) {
        return Err(traversal(&real, "a symlink points outside the root"));
    }
    if !real.is_dir() {
        return Err(Error::not_found(format!(
            "sandbox: workdir {} does not exist",
            clean.display()
        )));
    }
    Ok(real)
}

/// Resolve the configured writable paths.
///
/// A relative path is joined to the root; an absolute one is used as-is (an
/// exception outside the root is the whole point of the setting). Every entry
/// must exist, and an entry that is the root itself is only an error when the
/// root is pinned read-only — it would be silently redundant otherwise.
pub(crate) fn resolve_writable(
    root: &Path,
    paths: &[String],
    readonly_root: bool,
) -> Result<Vec<PathBuf>> {
    let root = real_root(root)?;
    let root = root.as_path();
    let mut resolved = Vec::with_capacity(paths.len());
    for (index, raw) in paths.iter().enumerate() {
        if raw.trim().is_empty() {
            return Err(Error::validation(format!(
                "sandbox: writable_paths[{index}] is empty"
            )));
        }
        check_text(Path::new(raw))?;
        let path = PathBuf::from(raw);
        let absolute = if path.is_absolute() {
            path
        } else {
            root.join(path)
        };
        let real = std::fs::canonicalize(&absolute).map_err(|error| {
            Error::validation(format!(
                "sandbox: writable_paths[{index}] {} is not usable: {error}",
                absolute.display()
            ))
        })?;
        if real == root {
            if readonly_root {
                return Err(Error::validation(format!(
                    "sandbox: writable_paths[{index}] is the root, which is pinned read-only"
                )));
            }
            // Redundant: the root is already writable.
            continue;
        }
        resolved.push(real);
    }
    Ok(resolved)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tempdir() -> tempfile::TempDir {
        tempfile::tempdir().expect("tempdir")
    }

    #[test]
    fn containment_is_component_wise() {
        let root = Path::new("/srv/box");
        assert!(within(root, Path::new("/srv/box")));
        assert!(within(root, Path::new("/srv/box/a/b")));
        assert!(!within(root, Path::new("/srv/box2")));
        assert!(!within(root, Path::new("/srv")));
    }

    #[test]
    fn normalize_collapses_dot_and_dotdot() {
        assert_eq!(normalize(Path::new("/a/./b/../c")), PathBuf::from("/a/c"));
    }

    #[test]
    fn the_root_must_exist_and_be_a_directory() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        assert!(root.is_absolute());
        assert!(resolve_root("/definitely/not/here").is_err());
        assert!(resolve_root("").is_err());

        let file = dir.path().join("file");
        std::fs::write(&file, b"x").expect("write");
        assert_eq!(
            resolve_root(file.to_str().expect("utf8"))
                .expect_err("not a directory")
                .kind(),
            crate::error::ErrorKind::Validation
        );
    }

    #[test]
    fn a_workdir_is_relative_to_the_root() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        std::fs::create_dir(root.join("sub")).expect("sub");
        let workdir = resolve_workdir(&root, Some(Path::new("sub"))).expect("workdir");
        assert_eq!(workdir, root.join("sub"));
        // No workdir means the root itself.
        assert_eq!(resolve_workdir(&root, None).expect("default"), root);
    }

    #[test]
    fn a_workdir_outside_the_root_is_forbidden() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        let error = resolve_workdir(&root, Some(Path::new("../elsewhere"))).expect_err("traversal");
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("path traversal"), "{error}");

        let absolute =
            std::fs::canonicalize(dir.path().parent().expect("parent")).expect("canonical");
        assert_eq!(
            resolve_workdir(&root, Some(&absolute))
                .expect_err("traversal")
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_symlink_escape_is_forbidden() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        let outside = tempdir();
        std::os::unix::fs::symlink(outside.path(), root.join("escape")).expect("symlink");
        let error = resolve_workdir(&root, Some(Path::new("escape"))).expect_err("traversal");
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("symlink"), "{error}");
    }

    #[test]
    fn a_control_character_in_a_path_is_rejected() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        assert!(
            resolve_workdir(&root, Some(Path::new("a\nb")))
                .expect_err("control character")
                .to_string()
                .contains("control character")
        );
    }

    #[test]
    fn a_missing_workdir_is_not_found() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        assert_eq!(
            resolve_workdir(&root, Some(Path::new("nope")))
                .expect_err("missing")
                .kind(),
            crate::error::ErrorKind::NotFound
        );
    }

    #[test]
    fn writable_paths_must_exist_and_skip_the_root() {
        let dir = tempdir();
        let root = resolve_root(dir.path().to_str().expect("utf8")).expect("root");
        std::fs::create_dir(root.join("out")).expect("out");

        let resolved =
            resolve_writable(&root, &["out".to_owned(), ".".to_owned()], false).expect("paths");
        assert_eq!(resolved, vec![root.join("out")]);

        assert!(
            resolve_writable(&root, &["missing".to_owned()], false).is_err(),
            "a missing writable path must fail the build"
        );
        assert_eq!(
            resolve_writable(&root, &[".".to_owned()], true)
                .expect_err("conflict")
                .kind(),
            crate::error::ErrorKind::Validation
        );
    }
}
