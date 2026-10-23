//! One spelling of "which paths a rule names".
//!
//! Two callers share it: [`crate::helpers::glob`], where a pattern selects files
//! to return, and [`crate::scoped`], where a pattern selects paths a grant lets
//! through. Sharing the compiler is what keeps a deny rule and a glob search
//! from disagreeing about what `**` means.
//!
//! Two rules are deliberate and differ from a naive glob:
//!
//! * `*` and `?` never cross a `/`; only `**` spans components. So `*.ext`
//!   matches a top-level file only, and `**/*.ext` is how a caller says "at any
//!   depth".
//! * a pattern ending in `/**` also names the directory itself, because a
//!   policy rule that says "everything under `secrets`" must not be sidestepped
//!   by addressing `secrets`. The Go side's matcher spells this case out
//!   explicitly; here it is one expansion, so both callers inherit it.
//!
//! Case is folded only where the filesystem folds it (`cfg!(windows)`), matching
//! the Go side: on a case-insensitive host a policy that ignored case would be
//! bypassable by spelling the path differently.

use flowcraft_error::{Error, Result};
use globset::{GlobBuilder, GlobMatcher, GlobSet, GlobSetBuilder};

/// Compile one pattern, for matching a single path at a time.
pub(crate) fn compile(pattern: &str) -> Result<GlobMatcher> {
    Ok(builder(pattern)?.compile_matcher())
}

/// Compile a policy's patterns into one set, expanding the `/**` self-match.
pub(crate) fn compile_set(patterns: &[String]) -> Result<GlobSet> {
    let mut set = GlobSetBuilder::new();
    for pattern in patterns {
        for expanded in expand(pattern) {
            set.add(builder(&expanded)?);
        }
    }
    set.build()
        .map_err(|error| invalid(&patterns.join(", "), &error))
}

fn builder(pattern: &str) -> Result<globset::Glob> {
    GlobBuilder::new(pattern)
        .literal_separator(true)
        .backslash_escape(false)
        .case_insensitive(cfg!(windows))
        .build()
        .map_err(|error| invalid(pattern, &error))
}

/// `dir/**` matches `dir` too, so "everything under `dir`" cannot be sidestepped
/// by naming `dir`.
fn expand(pattern: &str) -> Vec<String> {
    match pattern.strip_suffix("/**") {
        Some(prefix) if !prefix.is_empty() => vec![pattern.to_owned(), prefix.to_owned()],
        _ => vec![pattern.to_owned()],
    }
}

/// Normalize a caller path for matching: portable separators, no `.` segments,
/// no duplicate slashes. `..` segments are kept as written, so a rule that names
/// them can match them; refusing them is the provider's job, not the matcher's.
pub(crate) fn normalize(path: &str) -> String {
    use std::path::Component;

    let mut parts: Vec<String> = Vec::new();
    for component in std::path::Path::new(path).components() {
        match component {
            Component::Normal(part) => parts.push(part.to_string_lossy().into_owned()),
            Component::ParentDir => parts.push("..".to_owned()),
            Component::CurDir => {}
            Component::RootDir | Component::Prefix(_) => {}
        }
    }
    if path.starts_with('/') || path.starts_with('\\') {
        return format!("/{}", parts.join("/"));
    }
    parts.join("/")
}

fn invalid(pattern: &str, error: &globset::Error) -> Error {
    Error::validation(format!("workspace: invalid pattern {pattern:?}: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn matches(pattern: &str, path: &str) -> bool {
        compile(pattern).unwrap().is_match(normalize(path))
    }

    #[test]
    fn a_star_stays_inside_one_component() {
        assert!(matches("*.json", "top.json"));
        assert!(!matches("*.json", "nested/deep.json"));
        assert!(matches("**/*.json", "nested/deep.json"));
        assert!(matches("**/*.json", "top.json"));
    }

    #[test]
    fn a_trailing_double_star_also_names_the_directory() {
        let set = compile_set(&["secrets/**".to_owned()]).unwrap();
        assert!(set.is_match("secrets"));
        assert!(set.is_match("secrets/key"));
        assert!(!set.is_match("secrets2/key"));

        let nested = compile_set(&["**/node_modules/**".to_owned()]).unwrap();
        assert!(nested.is_match("node_modules"));
        assert!(nested.is_match("a/node_modules"));
        assert!(nested.is_match("a/node_modules/pkg/index.js"));
    }

    #[test]
    fn normalization_is_portable() {
        assert_eq!(normalize("./a/b"), "a/b");
        assert_eq!(normalize("a//b/"), "a/b");
        assert_eq!(normalize("a/../b"), "a/../b");
        assert_eq!(normalize("."), "");
    }

    #[test]
    fn an_invalid_pattern_is_a_validation_error() {
        assert_eq!(
            compile("a[b").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }
}
