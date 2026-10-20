//! `core/resource`'s file loader: one config file, under a base directory, with
//! a cap.
//!
//! core keeps this in `core/resource` beside `Source`; it is here because the
//! file secret store is its only consumer in the port so far, and it moves to
//! `flowcraft-resource` when that crate lands -- the rules do not change, and
//! the doc comment goes with the code.
//!
//! # What it refuses, in core's order
//!
//! A file source needs a base directory, its name must be relative, the name is
//! cleaned lexically and may not climb out (`..` exactly, or anything starting
//! with `../` -- `..a` is an ordinary name), the base directory has to open, the
//! target has to exist (not-found is its own refusal, which the secret store
//! turns into "no such secret"), and the content is capped at
//! [`DEFAULT_MAX_BYTES`].
//!
//! The *symlink* escape is refused too, by canonicalizing the target and
//! requiring it to stay under the canonical base -- core does the same with
//! `os.OpenRoot`. A symlinked base directory loses its name that way (on macOS
//! `/var` is `/private/var`), which is fine: both sides of the comparison are
//! canonical, and the path a caller wrote is what the sentence quotes.

use std::fs;
use std::io::ErrorKind as IoErrorKind;
use std::path::{Path, PathBuf};

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_utils::decode::go_quote;

use crate::{Result, validation};

/// `core/resource`'s `defaultMaxBytes`: one mebibyte.
pub const DEFAULT_MAX_BYTES: i64 = 1 << 20;

/// A settings subtree that is either inline content or a file reference.
///
/// core's `Source` also carries `{"embed": name}`, which resolves against an
/// injected `fs.FS`. The port has no embed consumer in this crate, so there is
/// no variant for it yet; the secret store only ever builds `File`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    /// Literal content, returned as-is.
    Inline(Vec<u8>),
    /// A path relative to the loader's base directory.
    File(String),
}

/// Materializes [`Source`] values against one base directory.
#[derive(Debug, Clone)]
pub struct Loader {
    base_dir: Option<PathBuf>,
    max_bytes: i64,
}

impl Loader {
    /// Returns a loader whose file sources resolve against `base_dir`.
    pub fn new(base_dir: impl Into<PathBuf>) -> Self {
        Self {
            base_dir: Some(base_dir.into()),
            max_bytes: DEFAULT_MAX_BYTES,
        }
    }

    /// Returns a loader with no base directory, so every file source is refused.
    pub fn without_base_dir() -> Self {
        Self {
            base_dir: None,
            max_bytes: DEFAULT_MAX_BYTES,
        }
    }

    /// Caps materialized content.
    ///
    /// Zero keeps [`DEFAULT_MAX_BYTES`] and a negative value disables the cap,
    /// which is `resource.WithMaxBytes`'s rule -- so "no cap asked for" is
    /// spelled `-1`, not `0`, and the zero value is safe.
    pub fn with_max_bytes(mut self, max_bytes: i64) -> Self {
        self.max_bytes = if max_bytes == 0 {
            DEFAULT_MAX_BYTES
        } else {
            max_bytes
        };
        self
    }

    /// Returns the base directory file sources resolve against, if any.
    pub fn base_dir(&self) -> Option<&Path> {
        self.base_dir.as_deref()
    }

    /// Materializes `source`.
    pub fn load(&self, source: &Source) -> Result<Vec<u8>> {
        match source {
            Source::Inline(data) => Ok(data.clone()),
            Source::File(name) => self.load_file(name),
        }
    }

    /// Materializes one file reference.
    pub fn load_file(&self, name: &str) -> Result<Vec<u8>> {
        let Some(base_dir) = &self.base_dir else {
            return Err(validation(
                "resource loader: file source requires a base dir",
            ));
        };
        if Path::new(name).is_absolute() {
            return Err(validation(format!(
                "resource loader: file source {} must be relative",
                go_quote(name)
            )));
        }
        let clean = clean_path(name);
        if clean == ".." || clean.starts_with("../") {
            return Err(forbidden(format!(
                "resource loader: file source {} escapes base dir",
                go_quote(name)
            )));
        }
        // `os.OpenRoot`'s failure is the base directory's, not the file's.
        let root = fs::canonicalize(base_dir)
            .map_err(|err| forbidden(format!("resource loader: open base dir: {err}")))?;
        let target = base_dir.join(&clean);
        let canonical = fs::canonicalize(&target).map_err(|err| {
            if err.kind() == IoErrorKind::NotFound {
                not_found(format!(
                    "resource loader: file {} not found",
                    go_quote(name)
                ))
            } else {
                forbidden(format!(
                    "resource loader: read file {}: {err}",
                    go_quote(name)
                ))
            }
        })?;
        // The canonical target has to stay inside the canonical base: a
        // symlink that points out is refused, which is what `os.OpenRoot`
        // does on core's side.
        if !canonical.starts_with(&root) {
            return Err(forbidden(format!(
                "resource loader: read file {}: path escapes from parent",
                go_quote(name)
            )));
        }
        let data = fs::read(&canonical).map_err(|err| {
            forbidden(format!(
                "resource loader: read file {}: {err}",
                go_quote(name)
            ))
        })?;
        if self.max_bytes >= 0 && data.len() as i64 > self.max_bytes {
            return Err(validation(format!(
                "resource loader: file {} exceeds {} bytes",
                go_quote(name),
                self.max_bytes
            )));
        }
        Ok(data)
    }
}

/// `filepath.Clean`, for the relative names a file source carries.
///
/// Go's rule: an empty name is `.`, empty and `.` elements are dropped, and a
/// `..` element cancels the element before it (or stays, at the front, which is
/// how the caller above detects an escape).
///
/// Absolute names never reach here -- [`Loader::load_file`] refuses them first,
/// which is why Go's rule for them (an absolute path is rooted, and `..` cannot
/// climb above `/`) has no counterpart below.
pub fn clean_path(name: &str) -> String {
    let mut parts: Vec<&str> = Vec::new();
    for element in name.split('/') {
        match element {
            "" | "." => {}
            ".." => match parts.last() {
                Some(&previous) if previous != ".." => {
                    parts.pop();
                }
                _ => parts.push(".."),
            },
            _ => parts.push(element),
        }
    }
    if parts.is_empty() {
        return ".".to_string();
    }
    parts.join("/")
}

/// `errdefs.Forbidden`: a path that tried to leave the base directory.
fn forbidden(message: String) -> Error {
    Error::new(ErrorKind::Forbidden, message)
}

/// `errdefs.NotFound`: a source that is not there.
fn not_found(message: String) -> Error {
    Error::new(ErrorKind::NotFound, message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_path_is_go_filepath_clean() {
        assert_eq!(clean_path(""), ".");
        assert_eq!(clean_path(".//a"), "a");
        assert_eq!(clean_path("a/./b"), "a/b");
        assert_eq!(clean_path("a/../b"), "b");
        assert_eq!(clean_path("a/../../b"), "../b");
        assert_eq!(clean_path("..a"), "..a");
        assert_eq!(clean_path("a/..a"), "a/..a");
        assert_eq!(clean_path(".."), "..");
        assert_eq!(clean_path("a/"), "a");
        assert_eq!(clean_path("../a/../.."), "../..");
        assert_eq!(clean_path("a/../../../b"), "../../b");
    }

    #[test]
    fn a_name_that_only_looks_like_an_escape_is_a_name() {
        // `..a` is an ordinary file name: the escape check is `..` exactly, or
        // a leading `../`, and core's is `filepath.Clean`'s answer too.
        let loader = Loader::new(".");
        let refusal = loader
            .load_file("..a")
            .err()
            .unwrap_or_else(|| panic!("`..a` should have been looked up, not refused"));
        assert_eq!(refusal.kind(), ErrorKind::NotFound);
    }

    #[test]
    fn inline_content_needs_no_base_dir() {
        let loader = Loader::without_base_dir();
        assert_eq!(
            loader
                .load(&Source::Inline(b"a: 1".to_vec()))
                .unwrap_or_else(|err| panic!("{err}")),
            b"a: 1"
        );
        let refusal = loader
            .load(&Source::File("a.yaml".to_string()))
            .err()
            .unwrap_or_else(|| panic!("a file source resolved without a base dir"));
        assert_eq!(
            refusal.to_string(),
            "resource loader: file source requires a base dir"
        );
    }
}
